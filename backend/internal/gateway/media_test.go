package gateway

import "testing"

func TestValidMediaKey(t *testing.T) {
	valid := "default/kb1/parsed/t1/images/fig1.png"
	if !validMediaKey(valid, "default") {
		t.Fatal("expected a parser-produced image key to be accepted")
	}
	if layout := "default/kb1/parsed/t1/layout.json"; !validMediaKey(layout, "default") {
		t.Fatal("expected the parsed layout key to be accepted")
	}
	for _, key := range []string{
		"",
		"other/kb1/parsed/t1/images/fig1.png",   // another tenant
		"default/kb1/parsed/t1/images/../x.png", // traversal
		"default/kb1/parsed/t1/fig1.png",        // missing images segment
		"default/kb1/parsed/t1/images/sub/fig1.png",
		"default/kb1/parsed/t1/images/fig1.txt", // not an image
		"default/kb1/upload/t1/images/fig1.png", // not the parsed prefix
		"default/kb1/parsed/t1/images/.png",
		"other/kb1/parsed/t1/layout.json",    // another tenant
		"default/kb1/parsed/t1/layout..json", // traversal-ish name
		"default/kb1/parsed/t1/other.json",   // only layout.json is exposed
		"default/kb1/upload/t1/layout.json",  // not the parsed prefix
	} {
		if validMediaKey(key, "default") {
			t.Errorf("validMediaKey(%q) unexpectedly accepted", key)
		}
	}
}
