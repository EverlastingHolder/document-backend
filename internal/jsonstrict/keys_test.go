package jsonstrict

import "testing"

func TestRejectDuplicateKeysAcrossJSONValue(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{name: "valid scalar root", data: `"value"`},
		{name: "valid array root", data: `[{"left":1},{"right":2}]`},
		{name: "nested object duplicate", data: `{"outer":{"key":1,"key":2}}`, wantErr: true},
		{name: "object in array duplicate", data: `{"items":[{"key":1,"key":2}]}`, wantErr: true},
		{name: "escaped equivalent names", data: `{"a":1,"\u0061":2}`, wantErr: true},
		{name: "trailing JSON value", data: `{} {}`, wantErr: true},
		{name: "malformed object", data: `{"key":`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := RejectDuplicateKeys([]byte(test.data))
			if (err != nil) != test.wantErr {
				t.Fatalf("RejectDuplicateKeys() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
