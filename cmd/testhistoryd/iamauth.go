// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/jackc/pgx/v5"
)

// iamAuthTarget returns the "host:port" that signs IAM auth tokens and the AWS
// region of that endpoint. It returns an empty endpoint for ordinary password
// authentication.
func iamAuthTarget(flagValue string, cc *pgx.ConnConfig) (endpoint, region string, err error) {
	host := flagValue
	if host == "" {
		if cc.Password != "" {
			return "", "", nil
		}
		host = cc.Host
	}
	endpoint = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		endpoint = net.JoinHostPort(host, strconv.Itoa(int(cc.Port)))
	}
	region = rdsRegion(host)
	if region == "" {
		if flagValue == "" {
			return "", "", nil
		}
		return "", "", fmt.Errorf("RDS IAM auth endpoint %q names no AWS region", flagValue)
	}
	return endpoint, region, nil
}

// iamAuthHook returns a pgxpool BeforeConnect hook and the endpoint that it
// signs for, or a nil hook for ordinary password authentication. The hook sets a
// fresh IAM auth token as the password of each new connection. A token expires
// after 15 minutes, and an open connection stays authenticated.
func iamAuthHook(ctx context.Context, flagValue string, cc *pgx.ConnConfig) (hook func(context.Context, *pgx.ConnConfig) error, endpoint string, err error) {
	endpoint, region, err := iamAuthTarget(flagValue, cc)
	if err != nil || endpoint == "" {
		return nil, "", err
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, "", fmt.Errorf("loading AWS configuration: %w", err)
	}
	return func(ctx context.Context, cc *pgx.ConnConfig) error {
		token, err := auth.BuildAuthToken(ctx, endpoint, region, cc.User, awsConfig.Credentials)
		if err != nil {
			return fmt.Errorf("building RDS IAM auth token: %w", err)
		}
		cc.Password = token
		return nil
	}, endpoint, nil
}

// rdsRegion returns the AWS region within an RDS endpoint hostname such as
// "ci-aurora-cluster.cluster-cabcd1234.us-east-1.rds.amazonaws.com". It returns
// "" if host is not an RDS endpoint.
func rdsRegion(host string) string {
	prefix, ok := strings.CutSuffix(strings.ToLower(host), ".rds.amazonaws.com")
	if !ok {
		return ""
	}
	dot := strings.LastIndex(prefix, ".")
	if dot == -1 {
		return ""
	}
	return prefix[dot+1:]
}
