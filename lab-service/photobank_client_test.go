package main

import "testing"

func TestChangedPhotoFieldsDetectsNewValue(t *testing.T) {
	before := map[string]any{}
	after := map[string]any{"photo_before_test": "https://example.com/a.jpg"}
	changed := changedPhotoFields(before, after)
	if got := changed["photo_before_test"]; got != "https://example.com/a.jpg" {
		t.Fatalf("expected new photo field to be reported, got %v", changed)
	}
	if len(changed) != 1 {
		t.Fatalf("expected exactly 1 changed field, got %d: %v", len(changed), changed)
	}
}

func TestChangedPhotoFieldsDetectsReplacedValue(t *testing.T) {
	before := map[string]any{"photo_after": "https://example.com/old.jpg"}
	after := map[string]any{"photo_after": "https://example.com/new.jpg"}
	changed := changedPhotoFields(before, after)
	if got := changed["photo_after"]; got != "https://example.com/new.jpg" {
		t.Fatalf("expected replaced photo field to be reported, got %v", changed)
	}
}

func TestChangedPhotoFieldsIgnoresUnchanged(t *testing.T) {
	before := map[string]any{"photo_before": "https://example.com/same.jpg"}
	after := map[string]any{"photo_before": "https://example.com/same.jpg"}
	changed := changedPhotoFields(before, after)
	if len(changed) != 0 {
		t.Fatalf("expected no changes for identical value, got %v", changed)
	}
}

func TestChangedPhotoFieldsIgnoresEmptyAndOtherKeys(t *testing.T) {
	before := map[string]any{}
	after := map[string]any{
		"photo_after_test": "",
		"mass_before":      1520,
	}
	changed := changedPhotoFields(before, after)
	if len(changed) != 0 {
		t.Fatalf("expected empty photo value and non-photo key to be ignored, got %v", changed)
	}
}

func TestChangedPhotoFieldsNilBefore(t *testing.T) {
	after := map[string]any{"photo_before": "https://example.com/a.jpg"}
	changed := changedPhotoFields(nil, after)
	if got := changed["photo_before"]; got != "https://example.com/a.jpg" {
		t.Fatalf("expected nil before map to be treated as no prior value, got %v", changed)
	}
}
