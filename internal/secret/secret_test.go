package secret

import (
	"testing"
)

func TestGeneratedPasswordIsValid(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword(16)
		if err != nil || len(p) != 16 {
			t.Fatal(p, err)
		}
		if err := ValidatePassword(p); err != nil {
			t.Fatalf("%q: %v", p, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	bad := []string{"short1A!", "alllowercaseletters", "ALLUPPERCASELETTERS", "123456789012345"}
	for _, p := range bad {
		if ValidatePassword(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := ValidatePassword("Abcdefghijk1"); err != nil {
		t.Error(err)
	}
}
