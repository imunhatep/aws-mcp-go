package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAuthModesAreExclusive(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "local", cfg: Config{Addr: ":3040"}},
		{name: "profiles only", cfg: Config{Addr: ":3040", Profiles: "dev,prod"}},
		{name: "assume role only", cfg: Config{Addr: ":3040", AssumeRole: true}},
		{
			name:    "profiles with assume role",
			cfg:     Config{Addr: ":3040", Profiles: "dev", AssumeRole: true},
			wantErr: "--profiles cannot be combined",
		},
		{
			name:    "profiles with assume role arns",
			cfg:     Config{Addr: ":3040", Profiles: "dev", AssumeRoleArns: "arn:aws:iam::111111111111:role/r"},
			wantErr: "--profiles cannot be combined",
		},
		{name: "missing addr", cfg: Config{}, wantErr: "--addr"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
