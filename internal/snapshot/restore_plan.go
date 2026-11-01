package snapshot

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strings"
)

const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeSymlink    = "120000"
	modeGitlink    = "160000"
)

type leaf struct{ path, mode, oid string }

type listing []leaf

// treeID matches git write-tree. A subdirectory sorts as name+"/", modes
// carry no leading zeros, and object IDs are raw bytes.
func treeID(newHash func() hash.Hash, leaves listing) string {
	return hex.EncodeToString(treeHash(newHash, "", leaves))
}

func treeHash(newHash func() hash.Hash, prefix string, leaves listing) []byte {
	type entry struct {
		name, key, mode string
		id              []byte
	}
	var entries []entry
	for i := 0; i < len(leaves); {
		name, _, inDir := strings.Cut(leaves[i].path[len(prefix):], "/")
		if !inDir {
			id, _ := hex.DecodeString(leaves[i].oid)
			entries = append(entries, entry{name, name, leaves[i].mode, id})
			i++
			continue
		}
		dir := prefix + name + "/"
		end := i
		for end < len(leaves) && strings.HasPrefix(leaves[end].path, dir) {
			end++
		}
		entries = append(entries, entry{name, name + "/", "40000", treeHash(newHash, dir, leaves[i:end])})
		i = end
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	var body bytes.Buffer
	for _, e := range entries {
		body.WriteString(e.mode + " " + e.name + "\x00")
		body.Write(e.id)
	}
	h := newHash()
	fmt.Fprintf(h, "tree %d\x00", body.Len())
	h.Write(body.Bytes())
	return h.Sum(nil)
}
