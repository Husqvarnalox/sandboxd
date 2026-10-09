package job

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// Job IDs are random version-4 UUIDs. They are used as path components under
// the state directory and the cgroup tree, so anything that is not the exact
// canonical lowercase form is rejected before it touches the filesystem.
var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewID returns a random UUIDv4 in canonical form.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:]), nil
}

// ValidID reports whether id is a canonical UUIDv4 produced by NewID.
func ValidID(id string) bool {
	return len(id) == 36 && idRe.MatchString(id)
}
