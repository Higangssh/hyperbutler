//go:build windows

package engine

import "testing"

// TestHyperVAdminsMembership: the account database and the token agree when nothing
// changed since sign-in, and a membership in the token is never "pending".
func TestHyperVAdminsMembership(t *testing.T) {
	inToken, err := HyperVAdminsInToken()
	if err != nil {
		t.Fatal(err)
	}
	inAccount, err := HyperVAdminsInAccount()
	if err != nil {
		t.Skipf("the local account database cannot be read here: %v", err)
	}
	if inToken && !inAccount {
		t.Errorf("in the token but not in the account database")
	}
	if inToken && HyperVAdminsSignInPending() {
		t.Error("pending although the token has the group")
	}
	t.Logf("token %v, account %v", inToken, inAccount)
}
