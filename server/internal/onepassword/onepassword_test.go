package onepassword_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/oldsj/mainloop/server/internal/onepassword"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

type fakeAPI struct {
	vaults []op.VaultOverview
	items  map[string][]op.ItemOverview
	full   map[string]op.Item
}

func (f fakeAPI) ListVaults(context.Context) ([]op.VaultOverview, error) { return f.vaults, nil }
func (f fakeAPI) ListItems(_ context.Context, v string) ([]op.ItemOverview, error) {
	return f.items[v], nil
}
func (f fakeAPI) GetItem(_ context.Context, _, id string) (op.Item, error) {
	it, ok := f.full[id]
	if !ok {
		return op.Item{}, errors.New("not found")
	}
	return it, nil
}

func fixture() fakeAPI {
	section := "sec-1"
	return fakeAPI{
		vaults: []op.VaultOverview{{ID: "v1", Title: "Apps"}, {ID: "v2", Title: "Twins"}, {ID: "v3", Title: "Twins"}},
		items: map[string][]op.ItemOverview{"v1": {
			{ID: "i1", Title: "widgets-db", State: op.ItemStateActive},
			{ID: "i2", Title: "old", State: op.ItemStateArchived},
			{ID: "i3", Title: "dup", State: op.ItemStateActive}, {ID: "i4", Title: "dup", State: op.ItemStateActive},
		}},
		full: map[string]op.Item{"i1": {
			ID: "i1", Title: "widgets-db", VaultID: "v1", Category: op.ItemCategoryDatabase, Version: 4,
			Sections: []op.ItemSection{{ID: section, Title: "Admin"}},
			Fields: []op.ItemField{
				{ID: "password", Title: "password", FieldType: op.ItemFieldTypeConcealed, Value: "fictional-value"},
				{ID: "f2", Title: "token", FieldType: op.ItemFieldTypeConcealed, Value: "t1", SectionID: &section},
				{ID: "f3", Title: "token", FieldType: op.ItemFieldTypeText, Value: "t2"},
			},
		}},
	}
}

// Given vault, item and field by title or id, Lookup returns metadata only; missing, archived
// and ambiguous names are ErrBadTarget with a message naming them.
func TestLookup(t *testing.T) {
	c := &onepassword.Client{API: fixture()}
	ctx := context.Background()
	m, err := c.Lookup(ctx, privileged.OPRef{Vault: "Apps", Item: "widgets-db", Field: "password"})
	if err != nil || m.ItemID != "i1" || m.Version != 4 || m.FieldType != "Concealed" || m.Empty {
		t.Fatalf("lookup = %+v, %v", m, err)
	}
	m, err = c.Lookup(ctx, privileged.OPRef{Vault: "v1", Item: "i1", Field: "f2"})
	if err != nil || m.Section != "Admin" || m.FieldTitle != "token" {
		t.Fatalf("lookup by id = %+v, %v", m, err)
	}
	for _, ref := range []privileged.OPRef{
		{Vault: "Nope", Item: "widgets-db", Field: "password"},
		{Vault: "Twins", Item: "widgets-db", Field: "password"},
		{Vault: "Apps", Item: "old", Field: "password"},
		{Vault: "Apps", Item: "dup", Field: "password"},
		{Vault: "Apps", Item: "widgets-db", Field: "token"},
		{Vault: "Apps", Item: "widgets-db", Field: "nope"},
	} {
		if _, err := c.Lookup(ctx, ref); !errors.Is(err, privileged.ErrBadTarget) {
			t.Errorf("%+v: err = %v", ref, err)
		} else if strings.Contains(err.Error(), "fictional-value") {
			t.Errorf("%+v: the error carries a value", ref)
		}
	}
}

// Value reads the field by id with the item's current version; a field that is gone is
// ErrBadTarget.
func TestValue(t *testing.T) {
	c := &onepassword.Client{API: fixture()}
	ctx := context.Background()
	m, err := c.Lookup(ctx, privileged.OPRef{Vault: "Apps", Item: "widgets-db", Field: "password"})
	if err != nil {
		t.Fatal(err)
	}
	got, v, err := c.Value(ctx, m)
	if err != nil || string(v) != "fictional-value" || got.Version != 4 {
		t.Fatalf("value = %+v %q %v", got, v, err)
	}
	m.FieldID = "gone"
	if _, _, err := c.Value(ctx, m); !errors.Is(err, privileged.ErrBadTarget) {
		t.Fatalf("err = %v", err)
	}
}
