package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// 打码字段序列化默认输出打码标记：忘了打码的输出路径也写不出原文。
func TestSecretRedactsByDefault(t *testing.T) {
	out, _ := json.Marshal(struct {
		Hash   Secret     `json:"hash"`
		Empty  Secret     `json:"empty"`
		Cred   SecretJSON `json:"cred"`
		NoCred SecretJSON `json:"no_cred"`
		Ptr    *Secret    `json:"ptr"`
	}{Hash: "argon2$secret", Cred: SecretJSON(`{"psk":"x"}`)})
	got := string(out)
	if strings.Contains(got, "secret") || strings.Contains(got, "psk") {
		t.Fatalf("原文不该出现在输出里：%s", got)
	}
	if got != `{"hash":"***","empty":"","cred":"***","no_cred":null,"ptr":null}` {
		t.Fatalf("打码输出的形状不对：%s", got)
	}
}

func TestSecretRoundTripAndReveal(t *testing.T) {
	var s struct {
		Hash Secret     `json:"hash"`
		Cred SecretJSON `json:"cred"`
	}
	if err := json.Unmarshal([]byte(`{"hash":"h1","cred":{"psk":"x"}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Hash.Reveal() != "h1" || string(s.Cred.Reveal()) != `{"psk":"x"}` {
		t.Fatalf("反序列化应当原样收下：%+v", s)
	}
	if s.Hash.IsRedacted() || s.Cred.IsRedacted() {
		t.Fatal("真值不该被判成打码标记")
	}
	if err := json.Unmarshal([]byte(`{"hash":"***","cred":"***"}`), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Hash.IsRedacted() || !s.Cred.IsRedacted() {
		t.Fatal("收到打码标记应当能识别（apply 时表示保持不变）")
	}
	if err := json.Unmarshal([]byte(`{"cred":{bad}`), &s); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
}

// 生成的 kind 结构体里，打码字段的类型都是 Secret / SecretJSON：把 User 的 status 序列化出去看不到密码哈希。
func TestGeneratedSecretsNeverSerializeInPlain(t *testing.T) {
	st := UserStatus{PasswordHash: "argon2$hash", TotpSecret: "JBSWY3DPEHPK3PXP", RecoveryCodes: SecretJSON(`["a","b"]`)}
	out, _ := json.Marshal(st)
	for _, leak := range []string{"argon2", "JBSWY", `"a"`} {
		if strings.Contains(string(out), leak) {
			t.Fatalf("User 的 status 序列化后不该含 %s：%s", leak, out)
		}
	}
	srv := ServerStatus{Token: "tok-secret-1", AgentToken: "agent-secret-2", PullToken: "pull-secret-3"}
	out, _ = json.Marshal(srv)
	if strings.Contains(string(out), "secret-") || !strings.Contains(string(out), `"token":"***"`) {
		t.Fatalf("Server 的令牌不该明文序列化：%s", out)
	}
	in := InboundSpec{TLS: SecretJSON(`{"private_key":"k"}`), Settings: SecretJSON(`{}`)}
	if out, _ = json.Marshal(in); strings.Contains(string(out), "private_key") {
		t.Fatalf("Inbound 的 tls 不该明文序列化：%s", out)
	}
}
