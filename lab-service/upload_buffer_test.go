package main

import (
	"testing"
	"time"
)

func TestS3StoreBufferPath(t *testing.T) {
	s := &S3Store{bufferDir: "/data/upload-buffer"}
	got := s.bufferPath("lab/abc123/main-photo.jpg")
	want := "/data/upload-buffer/lab/abc123/main-photo.jpg"
	if got != want {
		t.Fatalf("bufferPath = %q, want %q", got, want)
	}
}

func TestS3StoreCooldown(t *testing.T) {
	s := &S3Store{}
	if s.underCooldown() {
		t.Fatal("свежий S3Store не должен быть в остывании")
	}
	s.startCooldown()
	if !s.underCooldown() {
		t.Fatal("после startCooldown ожидалось остывание")
	}
	s.coolUntil = time.Now().Add(-time.Second) // симулируем истёкший срок
	if s.underCooldown() {
		t.Fatal("истёкшее остывание должно сбрасываться само")
	}
	s.startCooldown()
	s.clearCooldown()
	if s.underCooldown() {
		t.Fatal("clearCooldown должен снимать остывание немедленно")
	}
}
