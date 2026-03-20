package absdb

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestRIPEMD128(t *testing.T) {
	tests := []struct {
		input string
		hash  string
	}{
		{"", "cdf26213a150dc3ecb610f18f6b38b46"},
		{"a", "86be7afa339d0fc7cfc785e72f578d33"},
		{"abc", "c14a12199c66e4ba84636b0f69144c77"},
		{"message digest", "9e327b3d6e523062afc1132d7df9d1b8"},
		{"abcdefghijklmnopqrstuvwxyz", "fd2aa607f71dc8f510714922b371834e"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", "d1e959eb179c911faea4624c60c5c702"},
		{"12345678901234567890123456789012345678901234567890123456789012345678901234567890", "3f45ef194732c2dbb2c4a2c769795fa3"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ripemd128Sum([]byte(tt.input))
			want, _ := hex.DecodeString(tt.hash)

			if !bytes.Equal(got[:], want) {
				t.Errorf("ripemd128(%q) = %x, want %s", tt.input, got, tt.hash)
			}
		})
	}
}
