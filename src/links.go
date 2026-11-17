package main

// Sale -> provider reference links for tills whose core does not yet hold the
// reference itself (ut-docs#6438). The old design wrote one plugin-storage key
// per card sale; the host caps a plugin at 1,024 keys with no guest delete, so
// after ~1,000 sales every new link failed and refunds declined.
//
// The links now live in a FIXED set of blob:own files. Memory/disk budget
// (till-caches-must-be-memory-bounded): linkBuckets x linkBucketMaxBytes =
// 8 MiB at most, far under the 50 MiB default storage_mb, oldest line out
// first. Blobs have no key cap, and a blob write never needs a new storage key.
//
// Known residual: two settles landing in the same bucket at the same moment
// can lose one line (the replace is atomic, the read-modify-write is not).
// Effect: that sale's refund declines; it can never refund a wrong charge.
// Gone once core stores the reference (transaction_ref, host path).

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	linkBuckets        = 256
	linkBucketMaxBytes = 32 << 10
	maxLinkLine        = 1024
)

func bucketName(saleID string) string {
	sum := sha256.Sum256([]byte(saleID))
	return fmt.Sprintf("rl-%02x", sum[0])
}

type linkStore struct{ blobs blobStore }

func cleanField(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\t\r\n")
}

// add records saleID -> ref (replacing an earlier link for the sale).
func (l linkStore) add(saleID, ref string, at time.Time) error {
	if l.blobs == nil {
		return fmt.Errorf("no blob store")
	}
	if !cleanField(saleID) || !cleanField(ref) {
		return fmt.Errorf("sale id or reference unusable")
	}
	line := fmt.Sprintf("%d\t%s\t%s\n", at.Unix(), saleID, ref)
	if len(line) > maxLinkLine {
		return fmt.Errorf("link line too long")
	}
	name := bucketName(saleID)
	cur, _, err := l.blobs.get(name)
	if err != nil {
		return err // never overwrite a bucket we could not read
	}
	var kept []string
	for _, ln := range strings.Split(string(cur), "\n") {
		if ln == "" {
			continue
		}
		f := strings.SplitN(ln, "\t", 3)
		if len(f) == 3 && f[1] == saleID {
			continue
		}
		kept = append(kept, ln)
	}
	kept = append(kept, strings.TrimSuffix(line, "\n"))
	size := 0
	for _, k := range kept {
		size += len(k) + 1
	}
	for size > linkBucketMaxBytes && len(kept) > 1 {
		size -= len(kept[0]) + 1
		kept = kept[1:]
	}
	return l.blobs.put(name, []byte(strings.Join(kept, "\n")+"\n"))
}

// find returns the reference linked to saleID, the newest line winning.
func (l linkStore) find(saleID string) (string, bool) {
	if l.blobs == nil || !cleanField(saleID) {
		return "", false
	}
	cur, found, err := l.blobs.get(bucketName(saleID))
	if err != nil || !found {
		return "", false
	}
	lines := strings.Split(string(cur), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.SplitN(lines[i], "\t", 3)
		if len(f) == 3 && f[1] == saleID && f[2] != "" {
			if _, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				return f[2], true
			}
		}
	}
	return "", false
}
