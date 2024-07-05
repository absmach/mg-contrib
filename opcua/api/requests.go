// Copyright (c) Abstract Machines
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/absmach/magistrala/pkg/apiutil"

type browseReq struct {
	ServerURI      string
	Namespace      string
	Identifier     string
	IdentifierType string
}

func (req *browseReq) validate() error {
	if req.ServerURI == "" {
		return apiutil.ErrMissingID
	}

	return nil
}
