package render

import "fmt"

// Bytes renders n as a human-readable size using decimal (SI) units, matching
// what dpkg/apt print (e.g. "38.2 MB", not "36.4 MiB"), so a bundle summary
// reads the way an operator's apt output already does.
func Bytes(n int64) string {
	const unit = 1000.0
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for f := float64(n) / unit; f >= unit && exp < 4; f /= unit {
		div *= unit
		exp++
	}
	units := []string{"kB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.1f %s", float64(n)/div, units[exp])
}

// Plural renders "%d noun" or "%d nouns" without pulling in a pluralisation
// dependency; every count debark prints (packages, files, problems) is a
// plain English plural-with-s.
func Plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
