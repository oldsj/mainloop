package merge

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
)

// The project API serves ProtectedGlobs and GlobsVersion as the contract's MergePolicyView, whose
// version is a const and whose list has a maxItems: bumping one side without the other must fail
// here, without a database.
func TestProtectedGlobsMatchTheContract(t *testing.T) {
	t.Parallel()
	if !api.MergePolicyViewProtectedGlobsVersion(GlobsVersion).Valid() {
		t.Fatalf("GlobsVersion %d is not the contract's protected_globs_version", GlobsVersion)
	}
	if len(ProtectedGlobs) > 5 {
		t.Fatalf("%d protected globs; the contract's protected_globs has maxItems 5", len(ProtectedGlobs))
	}
}
