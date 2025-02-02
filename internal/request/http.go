// Copyright 2020-2021 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package request

import (
	"context"
	"fmt"
	h "net/http"
	"strings"

	"github.com/golang-jwt/jwt"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type authType int

const (
	bearerBased authType = iota
	certificateBased
	anonymousBased
)

type http struct {
	*h.Request
	usernameClaimField string
	client             client.Client
}

func NewHTTP(request *h.Request, usernameClaimField string, client client.Client) Request {
	return &http{Request: request, usernameClaimField: usernameClaimField, client: client}
}

func (h http) GetHTTPRequest() *h.Request {
	return h.Request
}

//nolint:funlen
func (h http) GetUserAndGroups() (username string, groups []string, err error) {
	switch h.getAuthType() {
	case certificateBased:
		pc := h.TLS.PeerCertificates
		if len(pc) == 0 {
			return "", nil, fmt.Errorf("no provided peer certificates")
		}

		username, groups = pc[0].Subject.CommonName, pc[0].Subject.Organization
	case bearerBased:
		if h.isJwtToken() {
			username, groups, err = h.processJwtClaims()

			break
		}

		username, groups, err = h.processBearerToken()
	case anonymousBased:
		return "", nil, fmt.Errorf("capsule does not support unauthenticated users")
	}
	// In case of error, we're blocking the request flow here
	if err != nil {
		return "", nil, err
	}
	// In case the requester is asking for impersonation, we have to be sure that's allowed by creating a
	// SubjectAccessReview with the requested data, before proceeding.
	if impersonateUser := h.Request.Header.Get("Impersonate-User"); len(impersonateUser) > 0 {
		ac := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:     "impersonate",
					Resource: "users",
					Name:     impersonateUser,
				},
				User:   username,
				Groups: groups,
			},
		}
		if err = h.client.Create(h.Request.Context(), ac); err != nil {
			return "", nil, err
		}

		if !ac.Status.Allowed {
			return "", nil, NewErrUnauthorized(fmt.Sprintf("the current user %s cannot impersonate the user %s", username, impersonateUser))
		}
		// The current user is allowed to perform authentication, allowing the override
		username = impersonateUser
	}

	if impersonateGroups := h.Request.Header.Values("Impersonate-Group"); len(impersonateGroups) > 0 {
		for _, impersonateGroup := range impersonateGroups {
			ac := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Verb:     "impersonate",
						Resource: "groups",
						Name:     impersonateGroup,
					},
					User:   username,
					Groups: groups,
				},
			}
			if err = h.client.Create(h.Request.Context(), ac); err != nil {
				return "", nil, err
			}

			if !ac.Status.Allowed {
				return "", nil, NewErrUnauthorized(fmt.Sprintf("the current user %s cannot impersonate the group %s", username, impersonateGroup))
			}

			if !sets.NewString(groups...).Has(impersonateGroup) {
				// The current user is allowed to perform authentication, allowing the override
				groups = append(groups, impersonateGroup)
			}
		}
	}

	return username, groups, nil
}

func (h http) processJwtClaims() (username string, groups []string, err error) {
	claims := h.getJwtClaims()

	if claims["iss"] == "kubernetes/serviceaccount" {
		username = claims["sub"].(string)
		groups = append(groups, "system:serviceaccounts", fmt.Sprintf("system:serviceaccounts:%s", claims["kubernetes.io/serviceaccount/namespace"]))

		return
	}

	u, ok := claims[h.usernameClaimField]
	if !ok {
		return "", nil, fmt.Errorf("missing users claim in JWT")
	}

	username = u.(string)

	g, ok := claims["groups"]
	if !ok {
		return "", nil, fmt.Errorf("missing groups claim in JWT")
	}

	for _, v := range g.([]interface{}) {
		groups = append(groups, v.(string))
	}

	return username, groups, nil
}

func (h http) processBearerToken() (username string, groups []string, err error) {
	token := h.bearerToken()
	tr := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}

	if err = h.client.Create(context.Background(), tr); err != nil {
		return "", nil, fmt.Errorf("cannot create TokenReview")
	}

	if statusErr := tr.Status.Error; len(statusErr) > 0 {
		return "", nil, fmt.Errorf("cannot verify the token due to error")
	}

	return tr.Status.User.Username, tr.Status.User.Groups, nil
}

func (h http) bearerToken() string {
	return strings.ReplaceAll(h.Header.Get("Authorization"), "Bearer ", "")
}

func (h http) getAuthType() authType {
	switch {
	case (h.TLS != nil) && len(h.TLS.PeerCertificates) > 0:
		return certificateBased
	case len(h.bearerToken()) > 0:
		return bearerBased
	default:
		return anonymousBased
	}
}

func (h http) getJwtClaims() jwt.MapClaims {
	parser := jwt.Parser{
		SkipClaimsValidation: true,
	}

	var token *jwt.Token

	var err error

	if token, _, err = parser.ParseUnverified(h.bearerToken(), jwt.MapClaims{}); err != nil {
		panic(err)
	}

	return token.Claims.(jwt.MapClaims)
}

func (h http) isJwtToken() bool {
	parser := jwt.Parser{
		SkipClaimsValidation: true,
	}
	_, _, err := parser.ParseUnverified(h.bearerToken(), jwt.MapClaims{})

	return err == nil
}
