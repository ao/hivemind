package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written as a Go duration string, such as "5s".
type Duration time.Duration

// D returns d as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes d as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`durations are strings such as "5s"`)
	}
	return d.parse(s)
}

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New(`durations are strings such as "5s"`)
	}
	return d.parse(n.Value)
}

// MarshalYAML writes d as a string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %v", s, err)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a number of bytes, written as an integer or with a suffix:
// k, M, G, T (powers of 1000) or Ki, Mi, Gi, Ti (powers of 1024).
type ByteSize int64

var sizeRE = regexp.MustCompile(`^([0-9]+)([kKMGT]i?)?[bB]?$`)

// ParseByteSize parses "512Mi", "1G" or "1048576".
func ParseByteSize(s string) (ByteSize, error) {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: use a whole number with an optional suffix such as 512Mi or 2G", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	mult := map[string]int64{
		"": 1, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"ki": 1 << 10, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
	}[m[2]]
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	return ByteSize(n * mult), nil
}

func (b ByteSize) String() string {
	for _, u := range []struct {
		s string
		n int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if b != 0 && int64(b)%u.n == 0 {
			return strconv.FormatInt(int64(b)/u.n, 10) + u.s
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// MarshalJSON writes b as a number of bytes.
func (b ByteSize) MarshalJSON() ([]byte, error) { return json.Marshal(int64(b)) }

// UnmarshalJSON reads a number of bytes or a size string.
func (b *ByteSize) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return errors.New(`sizes are numbers of bytes or strings such as "512Mi"`)
	}
	v, err := ParseByteSize(s)
	*b = v
	return err
}

// UnmarshalYAML reads a number of bytes or a size string.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New(`sizes are numbers of bytes or strings such as "512Mi"`)
	}
	v, err := ParseByteSize(n.Value)
	*b = v
	return err
}

// MarshalYAML writes b as a size string.
func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

// Image is a parsed container image reference.
type Image struct {
	Domain string // registry host, such as docker.io
	Path   string // repository path, such as library/nginx
	Tag    string // empty when Digest is set and no tag was given
	Digest string // sha256:<hex>, or empty
}

var (
	domainRE = regexp.MustCompile(`^(localhost|[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*)(:[0-9]{1,5})?$`)
	pathRE   = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ParseImage parses and normalises an image reference: "nginx" becomes
// docker.io/library/nginx:latest.
func ParseImage(ref string) (Image, error) {
	var img Image
	if ref == "" {
		return img, errors.New("required")
	}
	if len(ref) > maxImageLen {
		return img, fmt.Errorf("at most %d characters", maxImageLen)
	}
	rest := ref
	if i := strings.Index(rest, "@"); i >= 0 {
		img.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRE.MatchString(img.Digest) {
			return Image{}, fmt.Errorf("%q: digest must be sha256: and 64 lowercase hex digits", ref)
		}
	}
	// A tag follows the last colon, unless that colon is part of a registry host:port.
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i+1:], "/") {
		img.Tag = rest[i+1:]
		rest = rest[:i]
		if !tagRE.MatchString(img.Tag) {
			return Image{}, fmt.Errorf("%q: invalid tag %q", ref, img.Tag)
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost" || strings.ToLower(parts[0]) != parts[0]) {
		img.Domain = parts[0]
		parts = parts[1:]
		if !domainRE.MatchString(img.Domain) {
			return Image{}, fmt.Errorf("%q: invalid registry %q", ref, img.Domain)
		}
	} else {
		img.Domain = "docker.io"
	}
	for _, p := range parts {
		if !pathRE.MatchString(p) {
			return Image{}, fmt.Errorf("%q: invalid repository name", ref)
		}
	}
	if img.Domain == "docker.io" && len(parts) == 1 {
		parts = append([]string{"library"}, parts...)
	}
	img.Path = strings.Join(parts, "/")
	if img.Tag == "" && img.Digest == "" {
		img.Tag = "latest"
	}
	if len(img.String()) > maxImageLen {
		return Image{}, fmt.Errorf("at most %d characters once normalised", maxImageLen)
	}
	return img, nil
}

// Repository returns domain/path.
func (i Image) Repository() string { return i.Domain + "/" + i.Path }

// String returns the normalised reference.
func (i Image) String() string {
	s := i.Repository()
	if i.Tag != "" {
		s += ":" + i.Tag
	}
	if i.Digest != "" {
		s += "@" + i.Digest
	}
	return s
}
