// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRDSRegion(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"ci-aurora-cluster.cluster-cabcd1234.us-east-1.rds.amazonaws.com", "us-east-1"},
		{"CI-AURORA-CLUSTER.CLUSTER-CABCD1234.US-EAST-1.RDS.AMAZONAWS.COM", "us-east-1"},
		{"aurora-tailnetdb-1-cluster.cluster-ro-c1yay.eu-central-1.rds.amazonaws.com", "eu-central-1"},
		{"ci-aurora.testhistoryd.svc.cluster.local", ""},
		{"localhost", ""},
		{"rds.amazonaws.com", ""},
	}
	for _, tt := range tests {
		if got := rdsRegion(tt.host); got != tt.want {
			t.Errorf("rdsRegion(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestIAMAuthTarget(t *testing.T) {
	const (
		rdsHost   = "ci-aurora-cluster.cluster-cabcd1234.us-east-1.rds.amazonaws.com"
		proxyHost = "ci-aurora.testhistoryd.svc.cluster.local"
	)
	tests := []struct {
		name         string
		flagValue    string
		host         string
		password     string
		wantEndpoint string
		wantErr      bool
	}{
		{
			name:         "rds host without a password selects IAM auth",
			host:         rdsHost,
			wantEndpoint: rdsHost + ":5432",
		},
		{
			name:     "a password keeps password authentication",
			host:     rdsHost,
			password: "hunter2",
		},
		{
			name: "a non-RDS host keeps password authentication",
			host: "localhost",
		},
		{
			name:         "the flag names the endpoint behind a proxy",
			flagValue:    rdsHost + ":5432",
			host:         proxyHost,
			wantEndpoint: rdsHost + ":5432",
		},
		{
			name:         "the flag takes the DSN port when it names none",
			flagValue:    rdsHost,
			host:         proxyHost,
			wantEndpoint: rdsHost + ":5432",
		},
		{
			name:         "the flag overrides a password",
			flagValue:    rdsHost + ":5432",
			host:         rdsHost,
			password:     "hunter2",
			wantEndpoint: rdsHost + ":5432",
		},
		{
			name:      "a flag with no AWS region is an error",
			flagValue: proxyHost + ":5432",
			host:      proxyHost,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := &pgx.ConnConfig{}
			cc.Host = tt.host
			cc.Port = 5432
			cc.Password = tt.password
			endpoint, region, err := iamAuthTarget(tt.flagValue, cc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("iamAuthTarget() error = %v, want error %v", err, tt.wantErr)
			}
			if endpoint != tt.wantEndpoint {
				t.Errorf("iamAuthTarget() endpoint = %q, want %q", endpoint, tt.wantEndpoint)
			}
			wantRegion := ""
			if tt.wantEndpoint != "" {
				wantRegion = "us-east-1"
			}
			if region != wantRegion {
				t.Errorf("iamAuthTarget() region = %q, want %q", region, wantRegion)
			}
		})
	}
}

// TestIAMAuthTargetBehindAProxy runs a proxied deployment's DSN through
// pgxpool.ParseConfig, because the table above builds each pgx.ConnConfig by
// hand.
func TestIAMAuthTargetBehindAProxy(t *testing.T) {
	const (
		dsn      = "postgres://testhistory@ci-aurora.testhistoryd.svc.cluster.local:5432/testhistory?sslmode=require"
		endpoint = "ci-aurora-cluster.cluster-cabcd1234.us-east-1.rds.amazonaws.com:5432"
	)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := iamAuthTarget(endpoint, config.ConnConfig); err != nil || got != endpoint {
		t.Errorf("iamAuthTarget() = %q, %v, want %q", got, err, endpoint)
	}
	// The proxy's name carries no region, so the flag is mandatory here. Without
	// it the daemon sends an empty password, and RDS refuses the connection.
	if got, _, err := iamAuthTarget("", config.ConnConfig); err != nil || got != "" {
		t.Errorf("iamAuthTarget() without the flag = %q, %v, want no IAM auth", got, err)
	}
}
