package xrpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWarpStatusContainsNoSecrets(t *testing.T) {
	st := WarpInstallResult{
		WarpStatusResult: WarpStatusResult{
			Installed:     true,
			AccountType:   "plus",
			DeviceID:      "dev-1",
			PublicKey:     "pub-key",
			PeerPublicKey: DefaultWarpPeerPublicKey,
			Endpoint:      DefaultWarpEndpoint,
			AddressV4:     "172.16.0.2/32",
			Reserved:      []int{1, 2, 3},
			MTU:           DefaultWarpMTU,
			HasLicense:    true,
		},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, forbidden := range []string{"private_key", "access_token", "license_key"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("WarpInstallResult 绝不能包含敏感字段 %q：%s", forbidden, s)
		}
	}

	creds := WarpCredentialsResult{
		Installed:     true,
		PrivateKey:    "priv-secret",
		PeerPublicKey: DefaultWarpPeerPublicKey,
		Endpoint:      DefaultWarpEndpoint,
		AddressV4:     "172.16.0.2/32",
		Reserved:      []int{1, 2, 3},
		MTU:           DefaultWarpMTU,
	}
	rawCreds, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	var got WarpCredentialsResult
	if err := json.Unmarshal(rawCreds, &got); err != nil {
		t.Fatal(err)
	}
	if got.PrivateKey != "priv-secret" || len(got.Reserved) != 3 || got.Reserved[2] != 3 {
		t.Fatalf("WarpCredentialsResult 往返异常：%+v", got)
	}
}
