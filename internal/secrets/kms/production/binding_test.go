package production

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
)

var vectorRef = secrets.KEKRef{Provider: "aws", ID: "kodama-prod", Version: "v2"}

// Golden vector: the byte framing is persisted authenticated data, so it must
// never change for an existing deployment. Built independently of binding().
func Test_binding_golden_vector(t *testing.T) {
	b, err := binding(wctx(), vectorRef, "aws-kms-v1")
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, s := range []string{"kodama/scope-key-kms-binding/v1", "inst-1", "t_acme", "sk_abc", "aws", "aws-kms-v1", "kodama-prod", "v2"} {
		want = append(want, 0, 0, 0, byte(len(s)))
		want = append(want, s...)
	}
	want = append(want, 0, 0, 0, 0, 0, 0, 0, 3)
	if !bytes.Equal(b, want) {
		t.Fatalf("framing changed:\n%x\n%x", b, want)
	}
	if got := hex.EncodeToString(b); got != frozenVector {
		t.Fatalf("golden vector changed: %s", got)
	}
}

const frozenVector = "0000001f6b6f64616d612f73636f70652d6b65792d6b6d732d62696e64696e672f763100000006696e73742d3100000006745f61636d6500000006736b5f616263000000036177730000000a6177732d6b6d732d76310000000b6b6f64616d612d70726f640000000276320000000000000003"

func Test_binding_distinguishes_every_field(t *testing.T) {
	base, err := binding(wctx(), vectorRef, "aws-kms-v1")
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*secrets.WrapContext, *secrets.KEKRef, *string){
		"installation": func(c *secrets.WrapContext, _ *secrets.KEKRef, _ *string) { c.InstallationID = "inst-2" },
		"tenant":       func(c *secrets.WrapContext, _ *secrets.KEKRef, _ *string) { c.TenantID = "t_other" },
		"key id":       func(c *secrets.WrapContext, _ *secrets.KEKRef, _ *string) { c.KeyID = "sk_abd" },
		"generation":   func(c *secrets.WrapContext, _ *secrets.KEKRef, _ *string) { c.Generation = 4 },
		"provider":     func(_ *secrets.WrapContext, r *secrets.KEKRef, _ *string) { r.Provider = "gcp" },
		"kek id":       func(_ *secrets.WrapContext, r *secrets.KEKRef, _ *string) { r.ID = "kodama-dev" },
		"version":      func(_ *secrets.WrapContext, r *secrets.KEKRef, _ *string) { r.Version = "v1" },
		"format":       func(_ *secrets.WrapContext, _ *secrets.KEKRef, f *string) { *f = "gcp-kms-v1" },
	}
	for name, mutate := range mutations {
		wc, ref, format := wctx(), vectorRef, "aws-kms-v1"
		mutate(&wc, &ref, &format)
		got, err := binding(wc, ref, format)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Equal(got, base) {
			t.Errorf("changing %s does not change the binding", name)
		}
	}
}

func Test_binding_delimiter_collisions(t *testing.T) {
	a := wctx()
	a.InstallationID, a.KeyID = "ab", "c"
	b := wctx()
	b.InstallationID, b.KeyID = "a", "bc"
	x, _ := binding(a, vectorRef, "f")
	y, _ := binding(b, vectorRef, "f")
	if bytes.Equal(x, y) {
		t.Fatal("shifting a boundary between fields collides")
	}
	// Separator-looking content is data.
	c := wctx()
	c.InstallationID = "inst-1|t_acme"
	c.TenantID = "t_acme"
	z, _ := binding(c, vectorRef, "f")
	w, _ := binding(wctx(), vectorRef, "f")
	if bytes.Equal(z, w) {
		t.Fatal("content collides with the delimiter")
	}
}

func Test_binding_rejects_invalid_context(t *testing.T) {
	long := string(bytes.Repeat([]byte{'a'}, 257))
	for name, wc := range map[string]secrets.WrapContext{
		"empty installation": {TenantID: "t_acme", Generation: 1, KeyID: "k"},
		"long installation":  {InstallationID: long, TenantID: "t_acme", Generation: 1, KeyID: "k"},
		"empty tenant":       {InstallationID: "i", Generation: 1, KeyID: "k"},
		"bad tenant":         {InstallationID: "i", TenantID: "bad tenant!", Generation: 1, KeyID: "k"},
		"empty key":          {InstallationID: "i", TenantID: "t_acme", Generation: 1},
		"zero generation":    {InstallationID: "i", TenantID: "t_acme", KeyID: "k"},
		"negative":           {InstallationID: "i", TenantID: "t_acme", Generation: -1, KeyID: "k"},
		"control character":  {InstallationID: "i\n", TenantID: "t_acme", Generation: 1, KeyID: "k"},
	} {
		if _, err := binding(wc, vectorRef, "f"); err == nil || keyClass(t, err) != secrets.KeyCorrupt {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := binding(wctx(), secrets.KEKRef{Provider: "aws", ID: "", Version: "v"}, "f"); err == nil {
		t.Error("empty KEK id accepted")
	}
}
