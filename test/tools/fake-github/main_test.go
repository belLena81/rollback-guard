package main

import (
	"reflect"
	"testing"
)

func TestParseUsers(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantIDs []int64
		wantOrg [][]string
		wantErr bool
	}{
		{"several users with and without orgs", "admin:1001:demo+other; viewer:1003:demo;out:1004", []int64{1001, 1003, 1004}, [][]string{{"demo", "other"}, {"demo"}, nil}, false},
		{"empty", "", nil, nil, true},
		{"bad id", "a:x:o", nil, nil, true},
		{"zero id", "a:0:o", nil, nil, true},
		{"missing id", "a", nil, nil, true},
		{"too many parts", "a:1:o:x", nil, nil, true},
		{"duplicate login ignoring case", "a:1:o;A:2:o", nil, nil, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			users, err := parseUsers(test.spec)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			for i, user := range users {
				if user.ID != test.wantIDs[i] || !reflect.DeepEqual(user.Organizations, test.wantOrg[i]) {
					t.Errorf("user %d = %+v", i, user)
				}
			}
		})
	}
}
