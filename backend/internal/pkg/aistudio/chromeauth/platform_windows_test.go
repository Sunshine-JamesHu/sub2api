//go:build windows && cgo

package chromeauth

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverDoesNotOfferSignedOutCachedIdentity(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Default"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Default", "Preferences"), []byte(`{"account_info":[{"gaia":"123","email":"cached@example.test"}]}`), 0600))
	db, err := sql.Open("sqlite3", filepath.Join(root, "Default", "Web Data"))
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE token_service(service TEXT PRIMARY KEY, encrypted_token BLOB, binding_key BLOB)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO token_service VALUES(?,?,?)", "AccountId-123", []byte("v20-test-fixture"), []byte("binding-fixture"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	for _, test := range []struct {
		user   string
		active bool
	}{{"", false}, {"different@example.test", false}, {"cached@example.test", true}} {
		raw := []byte(`{"profile":{"info_cache":{"Default":{"name":"Fixture","user_name":"` + test.user + `"}}}}`)
		require.NoError(t, os.WriteFile(filepath.Join(root, "Local State"), raw, 0600))
		accounts, err := Discover(root)
		require.NoError(t, err)
		require.Len(t, accounts, 1)
		require.Equal(t, test.active, accounts[0].BrowserSignedIn)
		require.Equal(t, test.active, accounts[0].Importable)
		if !test.active {
			_, err = selectAccounts(accounts, nil, nil, []string{accounts[0].ID})
			require.Error(t, err)
		}
	}
}
