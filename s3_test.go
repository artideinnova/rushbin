package main

import (
	"testing"
	"time"
)

// TestPresign checks the signature against the example in AWS's documentation
// ("Authenticating Requests: Using Query Parameters", GET examplebucket/test.txt on 24 May 2013).
func TestPresign(t *testing.T) {
	s := &Store{Endpoint: "https://examplebucket.s3.amazonaws.com", Region: "us-east-1",
		KeyID: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	got := s.presign("GET", "test.txt", 24*time.Hour, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presign\n got %s\nwant %s", got, want)
	}
}
