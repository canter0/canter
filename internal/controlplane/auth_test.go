package controlplane

import (
	"errors"
	"testing"
)

func TestPasswordOpsRejectWhenCapacityIsFull(t *testing.T) {
	for i := 0; i < maxConcurrentPasswordOps; i++ {
		passwordOpSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(passwordOpSlots) > 0 {
			<-passwordOpSlots
		}
	})

	if err := enterPasswordOp(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("enterPasswordOp() error = %v, want ErrCapacity", err)
	}
	if _, err := hashPassword("correct horse battery staple"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("hashPassword() error = %v, want ErrCapacity", err)
	}
	if valid, err := verifyPasswordLimited(dummyPasswordHash, "password"); valid || !errors.Is(err, ErrCapacity) {
		t.Fatalf("verifyPasswordLimited() = (%v, %v), want (false, ErrCapacity)", valid, err)
	}
}

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
