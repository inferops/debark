package fake

import (
	"sort"
	"strconv"
	"strings"

	"github.com/inferops/debark/gui/internal/catalog"
)

// This file is the fake's data: a hand-written core of packages a demo or a
// screenshot will actually reach for, plus a generator that extends it to any
// size up to roughly eighty thousand entries.
//
// The generator exists because the numbers are the design. A picker that feels
// fine against two hundred rows and falls over against seventy thousand is the
// normal outcome of building a frontend against a stub, and every frontend
// package here builds against this package weeks before a real apt index is
// parsed. So the fake is sized like the real thing on request: fake.New(70000)
// produces a corpus with the same row count, the same shape of name, the same
// distribution of sections and roughly the same proportion of DEP-11
// applications as Ubuntu main + universe.
//
// Everything here is deterministic. An entry's attributes are derived from a
// hash of its own name, not from a running counter, so `python3-async-client`
// has the same version, section and size whether the corpus holds 500 entries
// or 70,000 — which is what makes a test written against a small corpus still
// mean something against a large one.

// DefaultEntryCount is the corpus size fake.New uses when the caller has no
// opinion. It is deliberately not 70,000: a test binary and a `wails dev`
// session should start instantly, and a frontend that wants the full weight
// asks for it. Roughly the size of Debian main for one architecture.
const DefaultEntryCount = 6000

// MaxEntryCount is the largest corpus the generator can produce without
// repeating a name. Asking for more yields this many. It is comfortably above
// the seventy thousand of Ubuntu main + universe, which is the number that
// matters.
var MaxEntryCount = len(nameStems) * totalFamilyWeight()

func totalFamilyWeight() int {
	n := 0
	for _, f := range nameFamilies {
		n += f.weight
	}
	return n
}

// curated is the set of packages someone will type into the search box while
// looking at the screen. Real names, real sections, plausible versions and
// summaries in the voice apt actually uses.
//
// Sizes are within a factor of two of reality, which is what matters: the
// selection tray's "this will download 1.4 GB" line should be believable, and
// a fake where every package is 100 kB makes it impossible to tell whether the
// formatting works.
type curatedPkg struct {
	name    string
	version string
	section string
	summary string
	// appName and cats are the DEP-11 additions. An empty appName means
	// DEP-11 does not describe this package, which is the ordinary case.
	appName string
	// cats is space-separated so the table stays one line per package.
	cats      string
	component string
	sizeKiB   int64
	homepage  string
}

var curated = []curatedPkg{
	// Desktop applications: the DEP-11 tier. These are what AppsOnly shows.
	{"firefox", "145.0+build1-0ubuntu1", "web", "Safe and easy web browser from Mozilla", "Firefox Web Browser", "Network WebBrowser", "main", 371520, "https://www.mozilla.org/firefox/"},
	{"thunderbird", "1:140.4.0+build1-0ubuntu1", "mail", "Email, RSS and newsgroup client with integrated spam filter", "Thunderbird Mail", "Network Email", "main", 246784, "https://www.thunderbird.net/"},
	{"chromium-browser", "1:85.0.4183.83-0ubuntu2", "web", "Transitional package - chromium-browser -> chromium snap", "Chromium Web Browser", "Network WebBrowser", "universe", 4096, ""},
	{"gimp", "3.0.4-1ubuntu2", "graphics", "GNU Image Manipulation Program", "GNU Image Manipulation Program", "Graphics RasterGraphics 2DGraphics", "universe", 18432, "https://www.gimp.org/"},
	{"inkscape", "1.4.2-1build3", "graphics", "vector-based drawing program", "Inkscape", "Graphics VectorGraphics 2DGraphics", "universe", 96256, "https://inkscape.org/"},
	{"libreoffice-writer", "4:25.2.5-0ubuntu1", "editors", "office productivity suite -- word processor", "LibreOffice Writer", "Office WordProcessor", "main", 34816, "https://www.libreoffice.org/"},
	{"libreoffice-calc", "4:25.2.5-0ubuntu1", "editors", "office productivity suite -- spreadsheet", "LibreOffice Calc", "Office Spreadsheet", "main", 27648, "https://www.libreoffice.org/"},
	{"libreoffice-impress", "4:25.2.5-0ubuntu1", "editors", "office productivity suite -- presentation", "LibreOffice Impress", "Office Presentation", "main", 13312, "https://www.libreoffice.org/"},
	{"vlc", "3.0.21-1build2", "video", "multimedia player and streamer", "VLC media player", "AudioVideo Player", "universe", 1536, "https://www.videolan.org/vlc/"},
	{"audacity", "3.7.3-1build1", "sound", "fast, cross-platform audio editor", "Audacity", "AudioVideo Audio AudioVideoEditing", "universe", 30720, "https://www.audacityteam.org/"},
	{"blender", "4.3.2+dfsg-1build2", "graphics", "Very fast and versatile 3D modeller/renderer", "Blender", "Graphics 3DGraphics", "universe", 337920, "https://www.blender.org/"},
	{"krita", "1:5.2.9+dfsg-1", "graphics", "pixel-based image manipulation program", "Krita", "Graphics RasterGraphics 2DGraphics", "universe", 210944, "https://krita.org/"},
	{"darktable", "5.0.1-1", "graphics", "virtual lighttable and darkroom for photographers", "darktable", "Graphics Photography", "universe", 92160, "https://www.darktable.org/"},
	{"kdenlive", "25.04.3-0ubuntu1", "video", "non-linear video editor", "Kdenlive", "AudioVideo AudioVideoEditing Video", "universe", 51200, "https://kdenlive.org/"},
	{"obs-studio", "31.0.2+dfsg-1build2", "video", "recorder and streamer for live video content", "OBS Studio", "AudioVideo Recorder", "universe", 20480, "https://obsproject.com/"},
	{"code", "1.104.2-1758741600", "devel", "Code editing. Redefined.", "Visual Studio Code", "Development IDE TextEditor", "universe", 389120, "https://code.visualstudio.com/"},
	{"remmina", "1.4.40+dfsg-1", "net", "GTK+ Remote Desktop Client", "Remmina", "Network RemoteAccess", "universe", 3072, "https://remmina.org/"},
	{"filezilla", "3.68.1-1", "net", "Full-featured graphical FTP/FTPS/SFTP client", "FileZilla", "Network FileTransfer", "universe", 12288, "https://filezilla-project.org/"},
	{"transmission-gtk", "4.0.6-1build3", "net", "lightweight BitTorrent client (GTK+ interface)", "Transmission", "Network FileTransfer P2P", "universe", 2048, "https://transmissionbt.com/"},
	{"keepassxc", "2.7.10+dfsg.1-1", "utils", "Cross Platform Password Manager", "KeePassXC", "Utility Security", "universe", 30720, "https://keepassxc.org/"},
	{"virt-manager", "5.0.0-2", "admin", "desktop application for managing virtual machines", "Virtual Machine Manager", "System Settings", "universe", 4096, "https://virt-manager.org/"},
	{"wireshark", "4.4.9-1", "net", "network traffic analyzer - meta-package", "Wireshark", "Network System Monitor", "universe", 128, "https://www.wireshark.org/"},
	{"gparted", "1.7.0-1", "admin", "GNOME partition editor", "GParted", "System Filesystem", "universe", 3072, "https://gparted.org/"},
	{"steam-installer", "1:1.0.0.83~ds-1", "games", "Valve's Steam digital software delivery system", "Steam", "Game", "multiverse", 2048, "https://store.steampowered.com/"},
	{"0ad", "0.27.1-1", "games", "Real-time strategy game of ancient warfare", "0 A.D.", "Game StrategyGame", "universe", 43008, "https://play0ad.com/"},
	{"supertuxkart", "1.4+ds-3build2", "games", "3D kart racing game", "SuperTuxKart", "Game ArcadeGame", "universe", 16384, "https://supertuxkart.net/"},
	{"scribus", "1.6.3+dfsg-1build3", "graphics", "Open Source Desktop Page Layout", "Scribus", "Office Publishing Graphics", "universe", 96256, "https://www.scribus.net/"},
	{"okular", "4:25.04.3-0ubuntu1", "graphics", "universal document viewer", "Okular", "Office Viewer", "universe", 12288, "https://okular.kde.org/"},
	{"evince", "48.0-1build1", "gnome", "Document (PostScript, PDF) viewer", "Document Viewer", "Office Viewer", "main", 4096, "https://apps.gnome.org/Evince/"},
	{"gnome-terminal", "3.56.2-1ubuntu1", "gnome", "GNOME terminal emulator application", "Terminal", "System TerminalEmulator", "main", 6144, "https://apps.gnome.org/Console/"},
	{"rhythmbox", "3.4.8-1build1", "sound", "music player and organizer for GNOME", "Rhythmbox", "AudioVideo Audio Player", "universe", 5120, "https://apps.gnome.org/Rhythmbox/"},
	{"shotwell", "0.32.10-1build1", "gnome", "digital photo organizer", "Shotwell", "Graphics Photography Viewer", "universe", 9216, "https://shotwell-project.org/"},
	{"meld", "3.22.3-1", "vcs", "graphical tool to diff and merge files", "Meld", "Development RevisionControl", "universe", 2048, "https://meld.app/"},
	{"dbeaver-ce", "25.1.5-0", "database", "Universal Database Manager", "DBeaver Community", "Development Database", "universe", 217088, "https://dbeaver.io/"},

	// The command-line core: no DEP-11, and the packages an experienced
	// operator types without looking.
	{"vim", "2:9.1.1230-1ubuntu1", "editors", "Vi IMproved - enhanced vi editor", "", "", "main", 3584, "https://www.vim.org/"},
	{"neovim", "0.11.4-1", "editors", "heavily refactored vim fork", "", "", "universe", 30720, "https://neovim.io/"},
	{"emacs", "1:30.1+1-2", "editors", "GNU Emacs editor (metapackage)", "GNU Emacs", "Development TextEditor", "universe", 64, "https://www.gnu.org/software/emacs/"},
	{"nano", "8.4-1", "editors", "small, friendly text editor inspired by Pico", "", "", "main", 1024, "https://www.nano-editor.org/"},
	{"git", "1:2.51.0-1ubuntu1", "vcs", "fast, scalable, distributed revision control system", "", "", "main", 26624, "https://git-scm.com/"},
	{"curl", "8.14.1-2ubuntu1", "web", "command line tool for transferring data with URL syntax", "", "", "main", 512, "https://curl.se/"},
	{"wget", "1.25.0-2", "web", "retrieves files from the web", "", "", "main", 3072, "https://www.gnu.org/software/wget/"},
	{"build-essential", "12.12ubuntu1", "devel", "Informational list of build-essential packages", "", "", "main", 20, ""},
	{"gcc", "4:14.2.0-1ubuntu1", "devel", "GNU C compiler", "", "", "main", 52, "https://gcc.gnu.org/"},
	{"g++", "4:14.2.0-1ubuntu1", "devel", "GNU C++ compiler", "", "", "main", 20, "https://gcc.gnu.org/"},
	{"make", "4.4.1-2", "devel", "utility for directing compilation", "", "", "main", 1536, "https://www.gnu.org/software/make/"},
	{"cmake", "3.31.6-2ubuntu1", "devel", "cross-platform, open-source make system", "", "", "main", 29696, "https://cmake.org/"},
	{"pkg-config", "1.8.1-4", "devel", "manage compile and link flags for libraries", "", "", "main", 128, ""},
	{"htop", "3.4.1-4", "utils", "interactive processes viewer", "", "", "main", 384, "https://htop.dev/"},
	{"btop", "1.4.0-1", "utils", "Modern and colorful command line resource monitor", "", "", "universe", 1024, "https://github.com/aristocratos/btop"},
	{"tmux", "3.5a-3", "misc", "terminal multiplexer", "", "", "main", 1024, "https://tmux.github.io/"},
	{"screen", "4.9.1-1build1", "misc", "terminal multiplexer with VT100/ANSI terminal emulation", "", "", "main", 1024, "https://www.gnu.org/software/screen/"},
	{"jq", "1.7.1-4", "utils", "lightweight and flexible command-line JSON processor", "", "", "main", 128, "https://jqlang.github.io/jq/"},
	{"ripgrep", "14.1.1-1", "utils", "Recursively searches directories for a regex pattern", "", "", "universe", 5120, "https://github.com/BurntSushi/ripgrep"},
	{"fd-find", "10.2.0-1", "utils", "Simple, fast and user-friendly alternative to find", "", "", "universe", 3072, "https://github.com/sharkdp/fd"},
	{"tree", "2.2.1-1", "utils", "displays an indented directory tree, in color", "", "", "main", 128, ""},
	{"rsync", "3.4.1+ds1-4", "net", "fast, versatile, remote (and local) file-copying tool", "", "", "main", 768, "https://rsync.samba.org/"},
	{"openssh-server", "1:10.0p1-7ubuntu1", "net", "secure shell (SSH) server, for secure access from remote machines", "", "", "main", 1536, "https://www.openssh.com/"},
	{"openssh-client", "1:10.0p1-7ubuntu1", "net", "secure shell (SSH) client, for secure access to remote machines", "", "", "main", 4096, "https://www.openssh.com/"},
	{"net-tools", "2.10-1.1ubuntu1", "net", "NET-3 networking toolkit", "", "", "main", 768, ""},
	{"iproute2", "6.14.0-1ubuntu1", "net", "networking and traffic control tools", "", "", "main", 3072, ""},
	{"dnsutils", "1:9.20.11-1ubuntu1", "net", "Transitional package for bind9-dnsutils", "", "", "main", 20, ""},
	{"nmap", "7.95+dfsg-4build1", "net", "The Network Mapper", "", "", "universe", 26624, "https://nmap.org/"},
	{"tcpdump", "4.99.5-2", "net", "command-line network traffic analyzer", "", "", "main", 1280, "https://www.tcpdump.org/"},
	{"unzip", "6.0-29ubuntu1", "utils", "De-archiver for .zip files", "", "", "main", 384, ""},
	{"zip", "3.0-14build1", "utils", "Archiver for .zip files", "", "", "main", 512, ""},
	{"p7zip-full", "16.02+dfsg-10build1", "utils", "7z and 7za file archivers with high compression ratio", "", "", "universe", 4096, ""},
	{"ca-certificates", "20250419", "misc", "Common CA certificates", "", "", "main", 384, ""},
	{"sudo", "1.9.16p2-1ubuntu1", "admin", "Provide limited super user privileges to specific users", "", "", "main", 5120, "https://www.sudo.ws/"},
	{"ufw", "0.36.2-9", "admin", "program for managing a Netfilter firewall", "", "", "main", 896, ""},
	{"fail2ban", "1.1.0-4", "net", "ban hosts that cause multiple authentication errors", "", "", "universe", 2560, "https://www.fail2ban.org/"},
	{"cron", "3.0pl1-192ubuntu1", "admin", "process scheduling daemon", "", "", "main", 256, ""},
	{"systemd", "257.4-1ubuntu3", "admin", "system and service manager", "", "", "main", 27648, "https://systemd.io/"},

	// Servers and databases.
	{"nginx", "1.28.0-2ubuntu1", "httpd", "small, powerful, scalable web/proxy server", "", "", "main", 64, "https://nginx.org/"},
	{"nginx-full", "1.28.0-2ubuntu1", "httpd", "nginx web/proxy server (standard version)", "", "", "main", 2048, "https://nginx.org/"},
	{"apache2", "2.4.65-1ubuntu1", "httpd", "Apache HTTP Server", "", "", "main", 2048, "https://httpd.apache.org/"},
	{"postgresql", "17+275", "database", "object-relational SQL database (supported version)", "", "", "main", 60, "https://www.postgresql.org/"},
	{"postgresql-17", "17.6-1", "database", "The World's Most Advanced Open Source Relational Database", "", "", "main", 47104, "https://www.postgresql.org/"},
	{"postgresql-client", "17+275", "database", "front-end programs for PostgreSQL (supported version)", "", "", "main", 60, "https://www.postgresql.org/"},
	{"mariadb-server", "1:11.8.3-0ubuntu1", "database", "MariaDB database server binaries", "", "", "main", 79872, "https://mariadb.org/"},
	{"redis-server", "5:7.4.5-1", "database", "Persistent key-value database with network interface", "", "", "universe", 128, "https://redis.io/"},
	{"sqlite3", "3.46.1-3", "database", "Command line interface for SQLite 3", "", "", "main", 2048, "https://sqlite.org/"},
	{"docker.io", "27.5.1-0ubuntu5", "admin", "Linux container runtime", "", "", "universe", 148480, "https://mobyproject.org/"},
	{"podman", "5.4.2+ds1-1", "admin", "engine to run OCI-based containers in Pods", "", "", "universe", 47104, "https://podman.io/"},
	{"qemu-system-x86", "1:10.0.2+ds-2ubuntu1", "otherosfs", "QEMU full system emulation binaries (x86)", "", "", "main", 66560, "https://www.qemu.org/"},

	// Language runtimes and the -dev packages that go with them.
	{"python3", "3.13.7-1", "python", "interactive high-level object-oriented language (default python3 version)", "", "", "main", 96, "https://www.python.org/"},
	{"python3-pip", "25.1.1+dfsg-1", "python", "Python package installer", "", "", "main", 3072, "https://pip.pypa.io/"},
	{"python3-venv", "3.13.7-1", "python", "venv module for python3 (default python3 version)", "", "", "main", 20, ""},
	{"python3-dev", "3.13.7-1", "python", "header files and a static library for Python (default)", "", "", "main", 20, ""},
	{"python3-requests", "2.32.4+dfsg-1", "python", "elegant and simple HTTP library for Python3, built for human beings", "", "", "main", 512, "https://requests.readthedocs.io/"},
	{"python3-numpy", "1:2.2.4+ds-1", "python", "Fast array facility to the Python 3 language", "", "", "main", 22528, "https://numpy.org/"},
	{"golang-go", "2:1.24~3", "golang", "Go programming language compiler - metapackage", "", "", "main", 20, "https://go.dev/"},
	{"nodejs", "20.19.5+dfsg-1ubuntu1", "javascript", "evented I/O for V8 javascript - runtime executable", "", "", "main", 141312, "https://nodejs.org/"},
	{"npm", "9.2.0~ds1-3", "javascript", "package manager for Node.js", "", "", "universe", 12288, "https://www.npmjs.com/"},
	{"default-jdk", "2:1.21-76", "java", "Standard Java or Java compatible Development Kit", "", "", "main", 20, ""},
	{"openjdk-21-jre", "21.0.8+9-0ubuntu1", "java", "OpenJDK Java runtime, using Hotspot JIT", "", "", "main", 448, ""},
	{"rustc", "1.85.0+dfsg-1", "devel", "Rust systems programming language", "", "", "universe", 12288, "https://www.rust-lang.org/"},
	{"cargo", "1.85.0+ds1-1", "devel", "Rust package manager", "", "", "universe", 15360, "https://doc.rust-lang.org/cargo/"},
	{"php-cli", "2:8.4+96ubuntu1", "php", "command-line interpreter for the PHP scripting language (default)", "", "", "main", 20, "https://www.php.net/"},
	{"ruby", "1:3.3", "ruby", "Interpreter of object-oriented scripting language Ruby (default version)", "", "", "main", 32, "https://www.ruby-lang.org/"},

	// A handful of real libraries, so the libs section is not entirely
	// synthetic and a search for "ssl" or "curl" finds what it should.
	{"libssl3t64", "3.5.1-1ubuntu1", "libs", "Secure Sockets Layer toolkit - shared libraries", "", "", "main", 6144, "https://www.openssl.org/"},
	{"libssl-dev", "3.5.1-1ubuntu1", "libdevel", "Secure Sockets Layer toolkit - development files", "", "", "main", 10240, "https://www.openssl.org/"},
	{"libcurl4t64", "8.14.1-2ubuntu1", "libs", "easy-to-use client-side URL transfer library (OpenSSL flavour)", "", "", "main", 768, "https://curl.se/"},
	{"libc6", "2.41-6ubuntu1", "libs", "GNU C Library: Shared libraries", "", "", "main", 13312, "https://www.gnu.org/software/libc/"},
	{"zlib1g", "1:1.3.dfsg+really1.3.1-1ubuntu1", "libs", "compression library - runtime", "", "", "main", 176, "https://zlib.net/"},
	{"libgtk-4-1", "4.18.6+ds-1", "libs", "GTK graphical user interface library", "", "", "main", 12288, "https://www.gtk.org/"},
	{"libwebkit2gtk-4.1-0", "2.48.5-1", "libs", "Web content engine library for GTK", "", "", "main", 133120, "https://webkitgtk.org/"},
	// The next summary is Debian's own wording for fonts-dejavu-core, its
	// spelling mistake included: this table quotes the archive rather than
	// improving on it, because what is searched here has to be what a search
	// over the real index would match.
	{"fonts-dejavu-core", "2.37-8", "fonts", "Vera font family derivate with additional characters", "", "", "main", 6144, ""}, //nolint:misspell // verbatim Debian description, see above
	{"fonts-noto-color-emoji", "2.047-1", "fonts", "color emoji font from Google", "", "", "main", 10240, ""},
}

// nameFamilies is how a synthesised long-tail name is shaped. Each family
// carries the section, the summary voice and the component that family really
// belongs to in a Debian archive, so grouping by section and filtering by
// component produce a realistic distribution rather than noise.
//
// The proportions are not equal: the library and language-binding families
// dominate a real archive and dominate here, which is the whole point — a
// search for "lib" must return tens of thousands of rows, because it does on a
// real machine, and the virtualiser has to survive that.
type nameFamily struct {
	// pattern places the stem: "%s" is substituted.
	pattern string
	section string
	// summary is a template with "%s" for a human-ish rendering of the stem.
	summary   string
	component string
	// weight is how many times this family is used per stem. Families with a
	// weight above one produce several distinct names from one stem by
	// appending a soname or a suffix.
	weight int
	// homepage is a template, or empty for a family that usually has none.
	homepage string
}

var nameFamilies = []nameFamily{
	{"lib%s", "libs", "%s shared library", "main", 3, ""},
	{"lib%s-dev", "libdevel", "development files for %s", "main", 3, ""},
	{"python3-%s", "python", "Python 3 module for %s", "universe", 3, "https://pypi.org/project/%s/"},
	{"golang-github-%s-dev", "golang", "Go library for %s", "universe", 2, "https://github.com/%s"},
	{"node-%s", "javascript", "Node.js module for %s", "universe", 2, "https://www.npmjs.com/package/%s"},
	{"lib%s-perl", "perl", "Perl module for %s", "universe", 1, ""},
	{"ruby-%s", "ruby", "Ruby library for %s", "universe", 1, ""},
	{"php-%s", "php", "PHP extension for %s", "universe", 1, ""},
	{"r-cran-%s", "gnu-r", "GNU R package for %s", "universe", 1, ""},
	{"haskell-%s-dev", "haskell", "Haskell library for %s", "universe", 1, ""},
	{"librust-%s-dev", "rust", "Rust crate providing %s", "universe", 2, ""},
	{"gir1.2-%s-1.0", "introspection", "GObject introspection data for %s", "universe", 1, ""},
	{"fonts-%s", "fonts", "%s font family", "universe", 1, ""},
	{"texlive-%s", "tex", "TeX Live: %s", "universe", 1, ""},
	{"%s-doc", "doc", "documentation for %s", "universe", 1, ""},
	{"%s-dbg", "debug", "debugging symbols for %s", "universe", 1, ""},
	{"%s-tools", "utils", "command line tools for %s", "universe", 1, ""},
	{"gnome-%s", "gnome", "GNOME %s", "universe", 1, ""},
	{"kde-%s", "kde", "KDE %s", "universe", 1, ""},
	{"%s-server", "net", "%s server daemon", "universe", 1, ""},
}

// nameHeads and nameTails are combined into stems. Two lists rather than one
// long one because a stem like "async-client" or "xml-parser" reads like a
// real package and a single word list runs out at a few hundred entries.
var nameHeads = []string{
	"async", "audio", "auth", "avahi", "batch", "binary", "boost", "cairo", "cbor", "cert",
	"cloud", "cluster", "codec", "colour", "compress", "config", "crypto", "cuda", "curses", "dbus",
	"deflate", "device", "digest", "dns", "docker", "edit", "event", "expat", "ffi", "filter",
	"font", "freetype", "gdal", "geo", "gettext", "glib", "graph", "grpc", "gtk", "hash",
	"http", "icu", "image", "index", "ini", "input", "ipc", "jpeg", "json", "kerberos",
	"lapack", "ldap", "lexer", "logging", "lua", "magick", "markdown", "matrix", "media", "mime",
	"mqtt", "netcdf", "nvidia", "oauth", "opengl", "openmp", "pango", "parser", "pcre", "pdf",
	"pixman", "plot", "png", "poppler", "proto", "pulse", "qt", "raster", "regex", "render",
	"rpc", "samba", "sasl", "schema", "sdl", "serial", "shell", "socket", "sqlite", "ssh",
	"stream", "systemd", "tesseract", "thread", "tiff", "timer", "tokenizer", "trace", "udev", "unicode",
	"usb", "utf8", "uuid", "vulkan", "wayland", "webp", "widget", "xml", "yaml", "zstd",
}

var nameTails = []string{
	"",
	"bridge", "client", "common", "core", "engine",
	"extra", "helper", "kit", "loader", "manager",
	"parser", "plugin", "proxy", "runtime", "server",
	"support", "tools", "utils", "widgets", "worker",
	"backend", "bindings", "cache", "codegen", "config",
	"daemon", "driver", "exporter", "format", "gateway",
	"handler", "iterator", "layout", "monitor", "notify",
}

// nameStems is every head/tail combination, in a fixed order. It is built once
// at init and never mutated, which is what makes the corpus deterministic
// without a seed argument on every call.
var nameStems = buildStems()

func buildStems() []string {
	out := make([]string, 0, len(nameHeads)*len(nameTails))
	seen := make(map[string]bool, len(nameHeads)*len(nameTails))
	for _, h := range nameHeads {
		for _, t := range nameTails {
			stem := h
			if t != "" {
				stem = h + "-" + t
			}
			// Deduplicated, because MaxEntryCount is derived from this
			// length and a duplicate stem would make it a lie: the generator
			// rejects the repeated name and silently produces a shorter
			// corpus than the caller asked for.
			if seen[stem] {
				continue
			}
			seen[stem] = true
			out = append(out, stem)
		}
	}
	return out
}

// appCategoryPool is the freedesktop categories a synthesised application may
// land in. Real category names, because Query.Category filters on them and a
// frontend that hard-codes "Graphics" must find rows there.
var appCategoryPool = [][]string{
	{"Development", "IDE"},
	{"Development", "Debugger"},
	{"Graphics", "RasterGraphics"},
	{"Graphics", "VectorGraphics"},
	{"Graphics", "Photography"},
	{"AudioVideo", "Player"},
	{"AudioVideo", "AudioVideoEditing"},
	{"Office", "WordProcessor"},
	{"Office", "Spreadsheet"},
	{"Office", "Viewer"},
	{"Network", "WebBrowser"},
	{"Network", "Email"},
	{"Network", "FileTransfer"},
	{"System", "Monitor"},
	{"System", "TerminalEmulator"},
	{"Utility", "TextEditor"},
	{"Utility", "Archiving"},
	{"Game", "ArcadeGame"},
	{"Game", "StrategyGame"},
	{"Education", "Science"},
}

// suitePool and its weights model where a version comes from. Most packages
// are still at the release version; a minority have moved to -updates and a
// few to -security. Entry.Suite exists so the picker can say so, and this is
// what makes that field non-trivial in the fake.
var suitePool = []string{"noble", "noble", "noble", "noble", "noble", "noble", "noble", "noble", "noble-updates", "noble-security"}

// buildCorpus returns n entries sorted by name: the curated table first (it is
// always present, whatever n is, because a demo must be able to find firefox),
// then generated long tail until the count is reached.
//
// n is clamped to [len(curated), MaxEntryCount]. Asking for fewer entries than
// the curated table holds returns the whole table rather than an arbitrary
// slice of it: a corpus missing `git` is a worse test fixture than one that is
// forty rows larger than requested.
func buildCorpus(n int) []catalog.Entry {
	if n > MaxEntryCount {
		n = MaxEntryCount
	}
	entries := make([]catalog.Entry, 0, max(n, len(curated)))
	seen := make(map[string]bool, max(n, len(curated)))

	for _, c := range curated {
		e := catalog.Entry{
			Name:      c.name,
			Version:   c.version,
			Section:   c.section,
			Summary:   c.summary,
			AppName:   c.appName,
			Component: c.component,
			Homepage:  c.homepage,
			Arch:      "amd64",
		}
		if c.cats != "" {
			e.Categories = strings.Fields(c.cats)
			e.IsApp = true
		}
		e.InstalledSizeKiB = c.sizeKiB
		decorate(&e)
		seen[e.Name] = true
		entries = append(entries, e)
	}

	// The generator walks stems in the outer loop and families in the inner
	// one, so a corpus truncated at any n covers every family. Truncating on
	// a family boundary would give a 2,000-entry corpus with no Python
	// packages in it, which is exactly the fixture that hides a bug.
	for si := 0; len(entries) < n && si < len(nameStems); si++ {
		stem := nameStems[si]
		for fi := range nameFamilies {
			if len(entries) >= n {
				break
			}
			f := nameFamilies[fi]
			for w := 0; w < f.weight && len(entries) < n; w++ {
				name := synthName(f, stem, w)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				entries = append(entries, synthEntry(f, stem, name))
			}
		}
	}

	sortByName(entries)
	return entries
}

// synthName renders one name from a family, a stem and a repetition index.
// The repetition index becomes a soname-ish suffix, which is how a real
// archive ends up with libfoo, libfoo1 and libfoo2 all present at once.
func synthName(f nameFamily, stem string, w int) string {
	s := stem
	if w > 0 {
		s = stem + strconv.Itoa(w)
	}
	return strings.ReplaceAll(f.pattern, "%s", s)
}

// synthEntry fills in one generated entry. Every attribute is derived from a
// hash of the entry's own name, so it does not depend on where in the corpus
// the entry landed or how large the corpus is.
func synthEntry(f nameFamily, stem, name string) catalog.Entry {
	h := fnv64(name)
	e := catalog.Entry{
		Name:      name,
		Section:   f.section,
		Component: f.component,
		Summary:   strings.ReplaceAll(f.summary, "%s", humanise(stem)),
		Arch:      "amd64",
	}
	if f.homepage != "" {
		e.Homepage = strings.ReplaceAll(f.homepage, "%s", stem)
	}
	// Documentation, fonts and TeX packages are architecture-independent, as
	// they are in a real archive.
	switch f.section {
	case "doc", "fonts", "tex":
		e.Arch = "all"
	}
	e.InstalledSizeKiB = int64(32 + h%40000)
	// Roughly one generated entry in seventy is a DEP-11 application, which
	// is close to the real ratio: Ubuntu publishes a few thousand desktop
	// applications against seventy thousand binary packages.
	if h%70 == 0 {
		e.IsApp = true
		e.AppName = titleCase(humanise(stem))
		e.Categories = appCategoryPool[h%uint64(len(appCategoryPool))]
	}
	decorate(&e)
	return e
}

// decorate fills in the fields every entry gets the same way — version, suite,
// priority and download size — from the entry's own name. Called for curated
// entries too, so the two halves of the corpus are indistinguishable to a
// caller except in the quality of their prose.
func decorate(e *catalog.Entry) {
	h := fnv64(e.Name)
	if e.Version == "" {
		e.Version = synthVersion(h)
	}
	if e.Suite == "" {
		e.Suite = suitePool[h%uint64(len(suitePool))]
	}
	if e.Component == "" {
		e.Component = "universe"
	}
	if e.Priority == "" {
		switch {
		case e.Section == "libs" && h%3 == 0:
			e.Priority = "required"
		case h%11 == 0:
			e.Priority = "important"
		case h%5 == 0:
			e.Priority = "standard"
		default:
			e.Priority = "optional"
		}
	}
	if e.DownloadSizeBytes == 0 {
		// A .deb is compressed: between a quarter and a half of the
		// installed size is the usual ratio, and the point of the field is
		// that "estimated download" and "disk needed" must not be the same
		// number on screen.
		ratio := 3 + h%3
		e.DownloadSizeBytes = e.InstalledSizeKiB * 1024 / int64(ratio)
		if e.DownloadSizeBytes < 1024 {
			e.DownloadSizeBytes = 1024 + int64(h%4096)
		}
	}
}

// synthVersion renders a plausible Debian version string. It is never parsed
// and never compared by anything — in this package or in the one it fakes —
// so it only has to look right.
func synthVersion(h uint64) string {
	major := h % 9
	minor := (h >> 3) % 20
	patch := (h >> 7) % 30
	rev := 1 + (h>>11)%4
	v := strconv.FormatUint(major, 10) + "." + strconv.FormatUint(minor, 10) + "." + strconv.FormatUint(patch, 10)
	switch (h >> 17) % 6 {
	case 0:
		return v + "-" + strconv.FormatUint(rev, 10)
	case 1:
		return v + "-" + strconv.FormatUint(rev, 10) + "build" + strconv.FormatUint(1+(h>>23)%3, 10)
	case 2:
		return v + "-" + strconv.FormatUint(rev, 10) + "ubuntu" + strconv.FormatUint(1+(h>>23)%3, 10)
	case 3:
		return v + "+dfsg-" + strconv.FormatUint(rev, 10)
	case 4:
		return "1:" + v + "-" + strconv.FormatUint(rev, 10)
	default:
		return v + "-" + strconv.FormatUint(rev, 10) + "ubuntu" + strconv.FormatUint(1+(h>>29)%2, 10)
	}
}

// humanise turns "xml-parser" into "xml parser" for use inside a summary.
func humanise(stem string) string { return strings.ReplaceAll(stem, "-", " ") }

// titleCase upper-cases the first letter of each word, for a synthesised
// application name. Deliberately not strings.Title (deprecated) and
// deliberately ASCII-only: every stem in this file is ASCII.
func titleCase(s string) string {
	b := []byte(s)
	up := true
	for i := range b {
		if up && b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
		up = b[i] == ' ' || b[i] == '-'
	}
	return string(b)
}

// fnv64 is FNV-1a, written out rather than imported so that the corpus cannot
// change because a stdlib hash changed its internals. Determinism across Go
// versions is the requirement: a golden test that pins a generated version
// string must keep passing.
func fnv64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// sortByName sorts entries by name ascending. One call site, so the corpus and
// every test agree on exactly one ordering — the same order Search promises.
func sortByName(entries []catalog.Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
