package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProfiles(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{name: "single", raw: "dev", want: []string{"dev"}},
		{name: "multiple with spaces", raw: "dev, prod ,sandbox", want: []string{"dev", "prod", "sandbox"}},
		{name: "empty entries dropped", raw: "dev,,prod,", want: []string{"dev", "prod"}},
		{name: "duplicate rejected", raw: "dev,prod,dev", wantErr: "duplicate aws profile"},
		{name: "no names", raw: " , ", wantErr: "no profile names"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProfiles(tc.raw)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
