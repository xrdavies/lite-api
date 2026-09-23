package app

import (
	"strings"
	"testing"
	"time"
)

func TestImageStorageConfigAndSigning(t *testing.T) {
	a := &App{secret: []byte(strings.Repeat("s", 32))}
	c := imageStorageConfig{
		Enabled:          true,
		Bucket:           "images",
		Prefix:           "generated/",
		Endpoint:         "https://objects.example.test",
		Region:           "auto",
		AccessKey:        "access",
		Secret:           "secret",
		PathStyle:        true,
		ExpiryHours:      12,
		MaxDownloadBytes: 1 << 20,
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if !c.complete() {
		t.Fatal("complete configuration was rejected")
	}
	encrypted, err := a.storageSecret(c.Secret, true)
	if err != nil || encrypted == c.Secret || !strings.HasPrefix(encrypted, "enc:v1:") {
		t.Fatalf("secret was not encrypted: %q %v", encrypted, err)
	}
	plain, err := a.storageSecret(encrypted, false)
	if err != nil || plain != c.Secret {
		t.Fatalf("secret round trip failed: %q %v", plain, err)
	}
	u, err := c.objectURL("generated/job 1/0.png")
	if err != nil || u.String() != "https://objects.example.test/images/generated/job%201/0.png" {
		t.Fatalf("object URL: %v %v", u, err)
	}
	if got, err := c.downloadURL("generated/job 1/0.png", time.Unix(1710000000, 0)); err != nil || !strings.Contains(got, "X-Amz-Signature=") || !strings.Contains(got, "X-Amz-Expires=43200") {
		t.Fatalf("presigned URL: %s %v", got, err)
	}
	for _, invalid := range []imageStorageConfig{
		{Bucket: "bad..bucket", Endpoint: c.Endpoint},
		{Bucket: "images", Endpoint: "http://objects.example.test", PublicBaseURL: "https://public.example.test/?token=x"},
		{Bucket: "images", Endpoint: c.Endpoint, Prefix: "../secrets"},
	} {
		if err := invalid.validate(); err == nil {
			t.Fatalf("invalid storage configuration accepted: %#v", invalid)
		}
	}
}
