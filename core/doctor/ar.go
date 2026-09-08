package doctor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// arMagic is the fixed 8-byte header every Unix ar archive starts with. .deb
// files are ar archives (Debian policy §5.6.1): debian-binary, control.tar.*,
// data.tar.* in that order, using the "common" ar variant dpkg-deb writes —
// short member names, no GNU extended-name table.
const arMagic = "!<arch>\n"

// maxArMembers bounds how many members parseAr will accept. Debian Policy
// §5.6.1 fixes a .deb at exactly three (debian-binary, control.tar.*,
// data.tar.*); a handful of signed .debs carry one or two _gpgorigin members
// besides. 64 is an order of magnitude above anything real, and stops an
// archive that is nothing but member headers from growing the members slice
// for as long as the file lasts.
const maxArMembers = 64

// maxControlMemberSize and maxDataMemberPrefix are the two byte budgets
// parseAr applies to a .deb's members, and they differ because doctor needs
// the two members for different things.
//
// The control member must be read WHOLE: silently truncating it could drop
// the very maintainer script whose absence would then read as "no obvious
// network action found" — a false clean result, the one outcome this package
// must never produce. So it gets a hard ceiling and parseAr refuses past it
// rather than truncating, exactly as core/fetch's maxControlSize does for the
// decompressed side (and at the same 8 MiB, ~1000x any real control member).
//
// The data member is only ever listed, best-effort, looking for
// usr/src/*/dkms.conf, and parseDebReader already stops after scanByteLimit
// DECOMPRESSED bytes of it. Buffering scanByteLimit COMPRESSED bytes
// therefore always yields at least as much decompressed data as that limit
// already allows doctor to read (no compressor expands its input), so taking
// only a prefix cannot change any result — while capping what a 1 GB data
// member costs in memory for a listing that stops after 8 MiB anyway.
const (
	maxControlMemberSize = 8 << 20 // 8 MiB
	maxDataMemberPrefix  = scanByteLimit
)

// maxArTotalBuffered bounds the dimension the two per-member budgets leave
// open between them: how much parseAr holds across ALL members at once. Each
// budget above is per member, and maxArMembers permits 64 of them, so the two
// multiply — measured, a 536,874,760-byte ar of 64 members each at the 8 MiB
// control budget returned 536,870,912 bytes of member data, 1,025 MiB of live
// heap and 2,048 MiB of cumulative allocation in 529ms. That is only ~2x the
// file that produced it rather than an expansion bomb, but it is 2x of a
// number the attacker picks, on a machine chosen for being air-gapped rather
// than for having a gigabyte to spare, from a file doctor reads two members
// of.
//
// 24 MiB is 1.5x the two members parseDebReader actually consumes
// (maxControlMemberSize for control.tar.*, maxDataMemberPrefix for
// data.tar.*), which leaves room for debian-binary's four bytes and the one
// or two _gpgorigin members a signed .deb carries while refusing anything
// that is stacking members to buy memory. No .deb dpkg-deb can build comes
// near it: the control member is already refused above 8 MiB and the data
// member is already truncated to 8 MiB however large the file is.
const maxArTotalBuffered = 24 << 20 // 24 MiB

// arMember is one file inside an ar archive.
type arMember struct {
	Name string
	Data []byte
	// Truncated is set when the member's declared size exceeded the budget
	// arMemberBudget gave it and Data therefore holds only a prefix. Only
	// ever true for members parseDebReader reads best-effort; a member it
	// must read whole is refused outright instead.
	Truncated bool
}

// arMemberBudget returns how many bytes of a member named name parseAr may
// buffer, and whether exceeding that budget is fatal (the member must be read
// whole) or merely truncating (a prefix is enough).
//
// parseAr is private to this package and only ever reads .deb files — the
// comment on arMagic already says so — so encoding which .deb member is which
// here is honest rather than a layering violation.
func arMemberBudget(name string) (limit int64, mustBeWhole bool) {
	switch {
	case strings.HasPrefix(name, "data.tar"):
		return maxDataMemberPrefix, false
	default:
		// debian-binary is four bytes; control.tar.* is a few KB. Anything
		// else is a member doctor does not read at all, and there is no
		// reason to let an unrecognised name buy a bigger buffer than the
		// one member that matters.
		return maxControlMemberSize, true
	}
}

// parseAr reads the members of an ar archive, buffering at most
// arMemberBudget(name) bytes of each. debark never needs to write ar
// archives.
//
// The data is untrusted: `debark doctor BUNDLE` reaches this on an
// extracted bundle that verify.Verify has not looked at, which is the whole
// point of the command (an operator runs it FIRST, on media that just
// arrived, to decide whether to trust it). Two properties follow from that
// and are what the loop below is shaped around:
//
//   - Nothing is ever allocated from a size an attacker declared. The member
//     size is a 10-character decimal field, so `make([]byte, size)` on the
//     declared value let a 69-byte file ask for 9,999,999,999 bytes: measured
//     at 10,000,759,040 bytes of heap and 10,022,283,512 bytes taken from the
//     OS in 17ms, which on any target with less committable memory than that
//     is a fatal, unrecoverable Go runtime out-of-memory rather than a
//     catchable error. Reading through io.CopyN into a growing buffer instead
//     makes a lying header cost only the bytes that actually exist. (This is
//     not only a hostile-input path: E7 hit a genuinely truncated 30 MB
//     package whose declared member size did not match its bytes.)
//   - A member that is longer than doctor could ever use is refused or
//     truncated per arMemberBudget, never read wholesale into memory.
func parseAr(r io.Reader) ([]arMember, error) {
	br := bufio.NewReader(r)

	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, fmt.Errorf("doctor: read ar magic: %w", err)
	}
	if string(magic) != arMagic {
		return nil, fmt.Errorf("doctor: not an ar archive (bad magic)")
	}

	var members []arMember
	var buffered int64
	header := make([]byte, 60)
	for {
		_, err := io.ReadFull(br, header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("doctor: read ar member header: %w", err)
		}
		if header[58] != '`' || header[59] != '\n' {
			return nil, fmt.Errorf("doctor: malformed ar member header (bad end marker)")
		}
		if len(members) == maxArMembers {
			return nil, fmt.Errorf("doctor: ar archive has more than %d members", maxArMembers)
		}
		name := strings.TrimRight(string(header[0:16]), " ")
		name = strings.TrimSuffix(name, "/") // defensive: tolerate GNU-style trailing slash
		sizeStr := strings.TrimSpace(string(header[48:58]))
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("doctor: malformed ar member size %q: %w", sizeStr, err)
		}
		if size < 0 {
			// A separate branch, not `err != nil || size < 0`: folding the
			// two together wrapped a nil error with %w and rendered the
			// message as "%!w(<nil>)".
			return nil, fmt.Errorf("doctor: negative ar member size %q", sizeStr)
		}

		limit, mustBeWhole := arMemberBudget(name)
		if size > limit && mustBeWhole {
			return nil, fmt.Errorf("doctor: ar member %q declares %d bytes, over the %d-byte limit (refusing rather than silently truncating it)", name, size, limit)
		}
		// Charge each member against a whole-archive budget as well as its
		// own, so 64 members each inside their per-member limit cannot add up
		// to 512 MiB. Capping the budget (rather than checking `size`) means a
		// truncating member is simply cut shorter when little is left, and a
		// must-be-whole member is refused below rather than silently losing
		// its tail.
		if remaining := maxArTotalBuffered - buffered; limit > remaining {
			limit = remaining
			if size > limit && mustBeWhole {
				return nil, fmt.Errorf("doctor: ar member %q needs %d bytes but the archive has already used %d of the %d-byte whole-archive budget (refusing rather than silently truncating it)", name, size, buffered, maxArTotalBuffered)
			}
		}
		want := size
		truncated := false
		if want > limit {
			want, truncated = limit, true
		}
		// io.CopyN into a growing buffer, never make([]byte, size): the size
		// comes from the archive, so the allocation must follow the bytes
		// that really arrive, not the number a header claims. See the
		// measured numbers on parseAr above.
		var buf bytes.Buffer
		n, err := io.CopyN(&buf, br, want)
		if err != nil {
			return nil, fmt.Errorf("doctor: read ar member %q data: %w", name, err)
		}
		if truncated {
			// Skip the rest of the member so the next header is found. The
			// bytes are discarded, not buffered.
			if _, err := io.CopyN(io.Discard, br, size-n); err != nil {
				return nil, fmt.Errorf("doctor: skip rest of ar member %q: %w", name, err)
			}
		}
		if size%2 == 1 {
			// Members are padded to an even length with a single byte.
			if _, err := br.Discard(1); err != nil && err != io.EOF {
				return nil, fmt.Errorf("doctor: discard ar padding after %q: %w", name, err)
			}
		}
		buffered += n
		members = append(members, arMember{Name: name, Data: buf.Bytes(), Truncated: truncated})
	}
	return members, nil
}
