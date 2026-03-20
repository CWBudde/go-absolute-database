package absdb

import "testing"

func TestCryptoHeaderEncryptedFile(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	if !db.Encrypted() {
		t.Fatal("expected Addresses.abs to be encrypted")
	}

	ch := db.CryptoHeader()
	if ch == nil {
		t.Fatal("expected non-nil CryptoHeader for encrypted file")
	}

	if ch.Algorithm != CryptoRijndael128 {
		t.Errorf("algorithm = %d, want CryptoRijndael128 (0)", ch.Algorithm)
	}

	if ch.HeaderSize != 280 {
		t.Errorf("header size = %d, want 280", ch.HeaderSize)
	}
}

func TestCryptoHeaderUnencryptedFile(t *testing.T) {
	db := openTestFile(t, "TS03.abs")

	if db.Encrypted() {
		t.Fatal("TS03.abs should not be encrypted")
	}

	ch := db.CryptoHeader()
	if ch != nil {
		t.Error("expected nil CryptoHeader for unencrypted file")
	}
}

func TestVerifyPassword(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	// Try both casings — the user wasn't sure which was used.
	blaOK := db.VerifyPassword("bla")
	BlaOK := db.VerifyPassword("Bla")

	if !blaOK && !BlaOK {
		t.Fatal("neither 'bla' nor 'Bla' verified as correct password")
	}

	if blaOK {
		t.Log("password is 'bla'")
	} else {
		t.Log("password is 'Bla'")
	}

	if db.VerifyPassword("wrong") {
		t.Error("wrong password should not verify")
	}
}

func TestOpenWithPassword(t *testing.T) {
	path := testdataPath("Addresses.abs")

	// Determine correct password.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	password := "bla"
	if !db.VerifyPassword(password) {
		password = "Bla"
	}
	db.Close()

	// OpenWithPassword with correct password.
	db, err = OpenWithPassword(path, password)
	if err != nil {
		t.Fatalf("OpenWithPassword(%q): %v", password, err)
	}
	defer db.Close()

	// Should be able to read schema.
	schema, err := db.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	if len(schema.Columns) == 0 {
		t.Error("expected columns in schema")
	}

	t.Logf("schema: %d columns", len(schema.Columns))
	for _, col := range schema.Columns {
		t.Logf("  %s (%v, size=%d)", col.Name, col.FieldType, col.Size)
	}
}

func TestOpenWithWrongPassword(t *testing.T) {
	path := testdataPath("Addresses.abs")

	_, err := OpenWithPassword(path, "wrong")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}

	if err != ErrWrongPassword {
		t.Errorf("expected ErrWrongPassword, got: %v", err)
	}
}

func TestEncryptedFileWithoutPassword(t *testing.T) {
	db := openTestFile(t, "Addresses.abs")

	// Without decryption key, encrypted pages won't have parseable ABSP headers.
	page, err := db.ReadPage(3) // page 3 has encrypted data
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}

	if page.Header != nil {
		t.Error("expected nil header for encrypted page without password")
	}
}
