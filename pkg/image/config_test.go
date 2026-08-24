package image

import "testing"

func TestParseAgodaConfig(t *testing.T) {
	raw := []byte(`{
		"mediatype": "application/vnd.agoda.macosvz.config.v1+json",
		"os": "darwin",
		"hardwareModelData": "abc",
		"machineIdData": "def",
		"storage": [
			{"mediatype": "application/vnd.agoda.macosvz.disk.image.v1", "file": "disk.img"},
			{"mediatype": "application/vnd.agoda.macosvz.aux.image.v1", "file": "aux.img"}
		]
	}`)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.MediaType != MediaTypeConfig {
		t.Fatalf("mediatype %s", c.MediaType)
	}
	if len(c.Storage) != 2 || c.Storage[0].MediaType != MediaTypeDisk || c.Storage[1].MediaType != MediaTypeAux {
		t.Fatalf("storage %+v", c.Storage)
	}
}

func TestParseMissingHardware(t *testing.T) {
	_, err := ParseConfig([]byte(`{"os":"darwin"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUncompressedDigest(t *testing.T) {
	if g := UncompressedDigestFromAnnotations(map[string]string{AgodaUncompDig: "sha256:x"}); g != "sha256:x" {
		t.Fatal(g)
	}
	if g := UncompressedDigestFromAnnotations(map[string]string{AnnotUncompressedDigest: "sha256:y"}); g != "sha256:y" {
		t.Fatal(g)
	}
}
