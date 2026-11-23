// Package onepassword reads 1Password items for serve-executor through a 1Password Service Account
// (read-only, scoped to the vaults the owner chose). It implements privileged.OnePassword: lookups
// return metadata only, and a value is read only to be written into a Secret.
package onepassword

import (
	"context"
	"fmt"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// API is the part of the SDK this package uses, so tests can fake it.
type API interface {
	ListVaults(ctx context.Context) ([]op.VaultOverview, error)
	ListItems(ctx context.Context, vaultID string) ([]op.ItemOverview, error)
	GetItem(ctx context.Context, vaultID, itemID string) (op.Item, error)
}

// Client implements privileged.OnePassword.
type Client struct {
	API API
}

// New signs in with a service account token.
func New(ctx context.Context, token, version string) (*Client, error) {
	c, err := op.NewClient(ctx, op.WithServiceAccountToken(token), op.WithIntegrationInfo("Mainloop executor", version))
	if err != nil {
		return nil, fmt.Errorf("onepassword: sign in: %w", err)
	}
	return &Client{API: sdk{c}}, nil
}

type sdk struct{ c *op.Client }

func (s sdk) ListVaults(ctx context.Context) ([]op.VaultOverview, error) {
	return s.c.Vaults().List(ctx)
}

func (s sdk) ListItems(ctx context.Context, vaultID string) ([]op.ItemOverview, error) {
	return s.c.Items().List(ctx, vaultID)
}

func (s sdk) GetItem(ctx context.Context, vaultID, itemID string) (op.Item, error) {
	return s.c.Items().Get(ctx, vaultID, itemID)
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{privileged.ErrBadTarget}, a...)...)
}

// Lookup finds the vault, item and field by id or exact title. The item read includes values;
// they are dropped here.
func (c *Client) Lookup(ctx context.Context, ref privileged.OPRef) (privileged.OPItem, error) {
	vaults, err := c.API.ListVaults(ctx)
	if err != nil {
		return privileged.OPItem{}, fmt.Errorf("onepassword: list vaults: %w", err)
	}
	var vault []op.VaultOverview
	for _, v := range vaults {
		if v.ID == ref.Vault || v.Title == ref.Vault {
			vault = append(vault, v)
		}
	}
	switch len(vault) {
	case 0:
		return privileged.OPItem{}, bad("no vault %q is visible to the executor's 1Password service account", ref.Vault)
	case 1:
	default:
		return privileged.OPItem{}, bad("%d vaults are named %q; name it by id", len(vault), ref.Vault)
	}
	items, err := c.API.ListItems(ctx, vault[0].ID)
	if err != nil {
		return privileged.OPItem{}, fmt.Errorf("onepassword: list items: %w", err)
	}
	var item []op.ItemOverview
	for _, it := range items {
		if it.State == op.ItemStateActive && (it.ID == ref.Item || it.Title == ref.Item) {
			item = append(item, it)
		}
	}
	switch len(item) {
	case 0:
		return privileged.OPItem{}, bad("vault %q has no item %q", vault[0].Title, ref.Item)
	case 1:
	default:
		return privileged.OPItem{}, bad("%d items in vault %q are named %q; name it by id", len(item), vault[0].Title, ref.Item)
	}
	full, err := c.API.GetItem(ctx, vault[0].ID, item[0].ID)
	if err != nil {
		return privileged.OPItem{}, fmt.Errorf("onepassword: read item: %w", err)
	}
	var field []op.ItemField
	for _, f := range full.Fields {
		if f.ID == ref.Field || f.Title == ref.Field {
			field = append(field, f)
		}
	}
	switch len(field) {
	case 0:
		return privileged.OPItem{}, bad("item %q has no field %q", full.Title, ref.Field)
	case 1:
	default:
		return privileged.OPItem{}, bad("%d fields of item %q are named %q; name it by id", len(field), full.Title, ref.Field)
	}
	return meta(vault[0].Title, full, field[0]), nil
}

// Value reads the field by id, with the item's current metadata.
func (c *Client) Value(ctx context.Context, want privileged.OPItem) (privileged.OPItem, []byte, error) {
	full, err := c.API.GetItem(ctx, want.VaultID, want.ItemID)
	if err != nil {
		return privileged.OPItem{}, nil, fmt.Errorf("onepassword: read item: %w", err)
	}
	for _, f := range full.Fields {
		if f.ID == want.FieldID {
			return meta(want.VaultTitle, full, f), []byte(f.Value), nil
		}
	}
	return privileged.OPItem{}, nil, bad("item %q has no field %q any more", full.Title, want.FieldTitle)
}

func meta(vaultTitle string, it op.Item, f op.ItemField) privileged.OPItem {
	m := privileged.OPItem{
		VaultID: it.VaultID, VaultTitle: vaultTitle, ItemID: it.ID, ItemTitle: it.Title, Category: string(it.Category),
		Version: it.Version, UpdatedAt: it.UpdatedAt.UTC(), FieldID: f.ID, FieldTitle: f.Title,
		FieldType: string(f.FieldType), Empty: f.Value == "",
	}
	if f.SectionID != nil {
		for _, s := range it.Sections {
			if s.ID == *f.SectionID {
				m.Section = s.Title
			}
		}
	}
	return m
}
