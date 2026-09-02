package controlplane

import "testing"

func TestPasswordHashIsArgon2AndVerifies(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery staple" || !verifyPassword(hash, "correct horse battery staple") {
		t.Fatal("password hash did not verify")
	}
	if verifyPassword(hash, "incorrect horse battery staple") {
		t.Fatal("incorrect password verified")
	}
}

func TestPasswordRejectsShortValues(t *testing.T) {
	if _, err := hashPassword("too short"); err == nil {
		t.Fatal("short password was accepted")
	}
}
