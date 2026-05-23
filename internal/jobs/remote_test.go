package jobs

import "testing"

func TestRegisterRemote(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterRemote("vendor.ShopifyImport", "localhost:50051")
	h := reg.Lookup("vendor.ShopifyImport")
	if h == nil {
		t.Fatal("expected remote handler to be registered")
	}
	if _, ok := h.(*RemoteHandler); !ok {
		t.Fatalf("expected *RemoteHandler, got %T", h)
	}
}

func TestRegisterRemote_OverwritesLocal(t *testing.T) {
	reg := NewRegistry()
	reg.Register("vendor.X", HandlerFunc(noopHandler))
	reg.RegisterRemote("vendor.X", "localhost:50051")
	h := reg.Lookup("vendor.X")
	if _, ok := h.(*RemoteHandler); !ok {
		t.Fatalf("expected remote handler to replace local, got %T", h)
	}
}
