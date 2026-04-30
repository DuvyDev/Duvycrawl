package utils

import "hash/fnv"

// HashURL computes a 64-bit FNV-1a hash of the given URL.
// The result is cast to int64 for efficient storage in SQLite INTEGER columns.
func HashURL(url string) int64 {
	h := fnv.New64a()
	h.Write([]byte(url))
	return int64(h.Sum64())
}
