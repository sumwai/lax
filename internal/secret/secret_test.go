package secret_test

import (
	"bytes"
	"testing"

	"lax/internal/secret"
)

func TestEncryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key, err := secret.LoadOrCreateKey(dir + "/secret.key")
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("密钥长度 = %d", len(key))
	}

	plain := []byte("sk-1234567890")
	blob, err := secret.Encrypt(key, plain)
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if bytes.Contains(blob, plain) {
		t.Fatal("密文里出现了明文")
	}

	back, err := secret.Decrypt(key, blob)
	if err != nil {
		t.Fatalf("解密: %v", err)
	}
	if !bytes.Equal(back, plain) {
		t.Fatalf("解密结果 = %q", back)
	}

	other, err := secret.LoadOrCreateKey(dir + "/other.key")
	if err != nil {
		t.Fatalf("生成另一把密钥: %v", err)
	}
	if _, err := secret.Decrypt(other, blob); err == nil {
		t.Fatal("用错密钥解密应当失败")
	}

	blob[len(blob)-1] ^= 0xFF
	if _, err := secret.Decrypt(key, blob); err == nil {
		t.Fatal("被改动的密文应当解密失败")
	}
}

func TestLoadOrCreateKeyIsStable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/secret.key"

	first, err := secret.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := secret.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("重复读取应返回同一把密钥")
	}
}

func TestMask(t *testing.T) {
	if got := secret.Mask("sk-abcdefgh"); got != "sk-a…" {
		t.Fatalf("Mask = %q", got)
	}
	if got := secret.Mask("abc"); got != "…" {
		t.Fatalf("短凭据 = %q", got)
	}
}
