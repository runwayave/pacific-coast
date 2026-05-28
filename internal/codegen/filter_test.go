package codegen

import (
	"strings"
	"testing"
)

func TestFilterIR_KeepsOnlyListedNamespaces(t *testing.T) {
	ir := lower(t, `
entity Product in vendor { id varchar(8) primary  title text not null }
entity Account in consumer { id bigint primary  email text not null }
`)
	out := FilterIR(ir, []string{"consumer"})
	if len(out.Entities) != 1 {
		t.Fatalf("entity count: got %d want 1", len(out.Entities))
	}
	if out.Entities[0].ID() != "consumer.Account" {
		t.Fatalf("kept wrong entity: %s", out.Entities[0].ID())
	}
}

// A consumer entity FK-referencing a vendor entity must not drag the vendor
// proto in: the FK is a scalar column, and the cross-namespace include
// variant must not be emitted for the dropped namespace (review Sev-2).
func TestFilterIR_DropsCrossNamespaceIncludeVariants(t *testing.T) {
	ir := lower(t, `
entity Product in vendor { id varchar(8) primary  title text not null }
entity SavedItem in consumer {
  id bigint primary
  product_id varchar(8) references vendor.Product.id
}
`)
	scoped := FilterIR(ir, []string{"consumer"})

	protos, err := EmitProto(scoped)
	if err != nil {
		t.Fatalf("EmitProto on filtered IR: %v", err)
	}
	for _, p := range protos {
		if strings.Contains(p.Path, "vendor") {
			t.Errorf("filtered output leaked vendor proto file: %s", p.Path)
		}
		// The scalar FK column is fine; a typed reference to the vendor
		// message or a vendor include variant is not.
		if strings.Contains(p.Content, "vendor.v1") || strings.Contains(p.Content, "VENDOR_PRODUCT") {
			t.Errorf("filtered proto %s leaked a vendor type reference", p.Path)
		}
	}
}

func TestFilterIR_NilIR(t *testing.T) {
	if FilterIR(nil, []string{"consumer"}) != nil {
		t.Fatal("FilterIR(nil) should return nil")
	}
}
