package database

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCredentialsEncryptAndAuthenticate(t *testing.T) {
	c, err := newCredentialCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	password := " secret'@:#/$\\password "
	one, err := c.seal(password)
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.seal(password)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(one, two) || bytes.Contains(one, []byte(password)) {
		t.Fatal("encryption reused nonce or exposed plaintext")
	}
	got, err := c.open(one)
	if err != nil || got != password {
		t.Fatal("password did not round-trip exactly")
	}
	wrongKey, _ := newCredentialCipher(bytes.Repeat([]byte{2}, 32))
	if _, err := wrongKey.open(one); err != ErrCredentials {
		t.Fatalf("wrong key error = %v", err)
	}
	tampered := append([]byte(nil), one...)
	tampered[len(tampered)-1] ^= 1
	for _, data := range [][]byte{nil, {1}, one[:12], tampered, append([]byte{2}, one[1:]...)} {
		if _, err := c.open(data); err != ErrCredentials {
			t.Fatalf("corrupt ciphertext error = %v", err)
		}
	}
	for _, key := range [][]byte{nil, make([]byte, 16), make([]byte, 31), make([]byte, 33)} {
		if _, err := newCredentialCipher(key); err != ErrInvalidConfig {
			t.Fatalf("invalid key error = %v", err)
		}
	}
}

func TestConnectionInputDoesNotSerializeOrFormatPassword(t *testing.T) {
	in := validInput()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(encoded), fmt.Sprintf("%v", in), fmt.Sprintf("%+v", in), fmt.Sprintf("%#v", in)} {
		if strings.Contains(output, *in.Password) {
			t.Fatal("input leaked password")
		}
	}
}

func TestConnectionInputValidation(t *testing.T) {
	for _, change := range []func(*ConnectionInput){
		func(in *ConnectionInput) { in.Name = " " },
		func(in *ConnectionInput) { in.Name = strings.Repeat("x", 101) },
		func(in *ConnectionInput) { in.Host = "localhost,attacker.example" },
		func(in *ConnectionInput) { in.Host = "/tmp" },
		func(in *ConnectionInput) { in.Host = "postgres://localhost" },
		func(in *ConnectionInput) { in.Host = "localhost:5432" },
		func(in *ConnectionInput) { in.Port = 0 },
		func(in *ConnectionInput) { in.Port = 65536 },
		func(in *ConnectionInput) { in.Database = "" },
		func(in *ConnectionInput) { in.Username = "user\x00name" },
		func(in *ConnectionInput) { in.SSLMode = "prefer" },
		func(in *ConnectionInput) { in.Password = nil },
		func(in *ConnectionInput) { in.Password = stringPointer("") },
		func(in *ConnectionInput) { in.Password = stringPointer("bad\x00password") },
	} {
		in := validInput()
		change(&in)
		if _, err := in.validate(true); err != ErrInvalidInput {
			t.Fatalf("invalid input error = %v", err)
		}
	}
	for _, host := range []string{"localhost", "db.example.com", "127.0.0.1", "::1"} {
		in := validInput()
		in.Host = host
		if _, err := in.validate(true); err != nil {
			t.Fatal(err)
		}
	}
	in := validInput()
	in.Password = nil
	if _, err := in.validate(false); err != nil {
		t.Fatal("update should allow retaining password")
	}
	for _, id := range []string{"", "-1", "0", "01", "1 OR 1=1", "9223372036854775808"} {
		if validID(databaseID(id)) {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
}
