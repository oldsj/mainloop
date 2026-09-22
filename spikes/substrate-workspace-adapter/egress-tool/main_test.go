package main

import "testing"

func TestValidateEgressInputRequiresExactlyOneMode(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string
		denyAll  bool
		allowAll bool
		wantErr  bool
	}{
		{name: "deny all", denyAll: true},
		{name: "cidr", cidr: "192.0.2.1/32"},
		{name: "allow all", allowAll: true},
		{name: "missing mode", wantErr: true},
		{name: "conflicting modes", denyAll: true, allowAll: true, wantErr: true},
		{name: "cidr and deny all", cidr: "192.0.2.1/32", denyAll: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEgressInput(tt.cidr, tt.denyAll, tt.allowAll)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateEgressInput() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
