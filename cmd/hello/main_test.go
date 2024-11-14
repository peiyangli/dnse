package main

import (
	"log"
	"testing"
)

func TestFileRef(t *testing.T) {
	x := []byte{3, 0, 0, 1, 6, 103, 33, 208, 200, 52, 221, 225, 61, 45, 14, 219, 36, 252, 24, 13, 196, 56, 225, 220, 15}
	xs := string(x)
	log.Println(xs)
}
