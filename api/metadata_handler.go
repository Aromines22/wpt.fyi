// Copyright 2018 The WPT Dashboard Project. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/v28/github"
	"github.com/web-platform-tests/wpt.fyi/api/query"
	"github.com/web-platform-tests/wpt.fyi/shared"
)

// MetadataHandler is an http.Handler for /api/metadata endpoint.
type MetadataHandler struct {
	logger      shared.Logger
	httpClient  *http.Client
	metadataURL string
}

// apiMetadataHandler searches Metadata for given products.
func apiMetadataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "Invalid HTTP method", http.StatusBadRequest)
		return
	}

	ctx := shared.NewAppEngineContext(r)
	client := shared.NewAppEngineAPI(ctx).GetHTTPClient()
	logger := shared.GetLogger(ctx)
	metadataURL := shared.MetadataArchiveURL
	delegate := MetadataHandler{logger, client, metadataURL}

	// Serve cached with 5 minute expiry. Delegate to Metadata Handler on cache miss.
	shared.NewCachingHandler(
		ctx,
		delegate,
		shared.NewGZReadWritable(shared.NewMemcacheReadWritable(ctx, 5*time.Minute)),
		shared.AlwaysCachable,
		cacheKey,
		shared.CacheStatusOK).ServeHTTP(w, r)
}

func apiMetadataTriageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PATCH" {
		http.Error(w, "Invalid HTTP method; only accpet PATCH request", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		http.Error(w, "Invalid content-type: %s"+contentType, http.StatusBadRequest)
		return
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read PATCH request body", http.StatusInternalServerError)
		return
	}

	err = r.Body.Close()
	if err != nil {
		http.Error(w, "Failed to finish reading request body", http.StatusInternalServerError)
		return
	}

	var metadata shared.MetadataResults
	err = json.Unmarshal(data, &metadata)
	if err != nil {
		http.Error(w, "Failed to parse JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx := shared.NewAppEngineContext(r)
	aeAPI := shared.NewAppEngineAPI(ctx)
	client := aeAPI.GetHTTPClient()
	logger := shared.GetLogger(ctx)
	ds := shared.NewAppEngineDatastore(ctx, false)
	user, token := shared.GetUserFromCookie(ctx, ds, r)
	if user == nil || token == nil {
		http.Error(w, "User is not logged in", http.StatusBadRequest)
		return
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: *token})
	tc := oauth2.NewClient(ctx, ts)
	githubClient := github.NewClient(tc)
	clientID, err := shared.GetSecret(ds, "github-oauth-client-id")
	if err != nil {
		http.Error(w, "Failed to get github-oauth-client-id secret: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Check if the token is still valid.
	_, res, err := githubClient.Authorizations.Check(ctx, clientID, *token)
	if err != nil {
		http.Error(w, "Fail to validate user token "+err.Error(), http.StatusInternalServerError)
		return
	}

	if res.StatusCode != http.StatusOK {
		http.Error(w, "User token invalid; please log in again. ", http.StatusBadRequest)
		return
	}

	_, res, err = githubClient.Organizations.GetOrgMembership(ctx, "", "web-platform-tests")
	if err != nil {
		http.Error(w, "Fail to validate web-platform-tests membership: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if res.StatusCode != http.StatusOK {
		http.Error(w, "User is not a part of web-platform-tests", http.StatusBadRequest)
		return
	}

	githubBotClient, err := getWPTFYIGithubBot(ctx)
	if err != nil {
		http.Error(w, "Unable to get Github Client: "+err.Error(), http.StatusInternalServerError)
		return
	}

	//TODO: Check github client permission levels for auto merge.
	git := metadataGithub{githubClient: githubBotClient, authorName: user.GitHubHandle, authorEmail: user.GithuhEmail}
	tm := triageMetadata{ctx: ctx, metadataGithub: git, logger: logger, httpClient: client}
	err = tm.triage(metadata)
	if err != nil {
		http.Error(w, "Unable to triage metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (h MetadataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var abstractLink query.AbstractLink
	if r.Method == "POST" {
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusInternalServerError)
			return
		}

		err = r.Body.Close()
		if err != nil {
			http.Error(w, "Failed to finish reading request body", http.StatusInternalServerError)
			return
		}

		var ae query.AbstractExists
		err = json.Unmarshal(data, &ae)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var isLinkQuery = false
		if len(ae.Args) == 1 {
			abstractLink, isLinkQuery = ae.Args[0].(query.AbstractLink)
		}

		if !isLinkQuery {
			h.logger.Errorf("Error from request: non Link search query %s for api/metadata", ae)
			http.Error(w, "Error from request: non Link search query for api/metadata", http.StatusBadRequest)
			return
		}
	}

	q := r.URL.Query()
	productSpecs, err := shared.ParseProductOrBrowserParams(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if len(productSpecs) == 0 {
		http.Error(w, fmt.Sprintf("Missing required 'product' param"), http.StatusBadRequest)
		return
	}

	metadataResponse, err := shared.GetMetadataResponseOnProducts(productSpecs, h.httpClient, h.logger, h.metadataURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method == "POST" {
		metadataResponse = filterMetadata(abstractLink, metadataResponse)
	}
	marshalled, err := json.Marshal(metadataResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = w.Write(marshalled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// filterMetadata filters the given Metadata down to entries where the value (links) contain
// at least one link where the URL contains the substring provided in the "link" search atom.
func filterMetadata(linkQuery query.AbstractLink, metadata shared.MetadataResults) shared.MetadataResults {
	res := make(shared.MetadataResults)
	for test, links := range metadata {
		for _, link := range links {
			if strings.Contains(link.URL, linkQuery.Pattern) {
				res[test] = links
				break
			}
		}
	}
	return res
}

// TODO(kyleju): Refactor this part to shared package.
var cacheKey = func(r *http.Request) interface{} {
	if r.Method == "GET" {
		return shared.URLAsCacheKey(r)
	}

	body := r.Body
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		msg := fmt.Sprintf("Failed to read non-GET request body for generating cache key: %v", err)
		shared.GetLogger(shared.NewAppEngineContext(r)).Errorf(msg)
		panic(msg)
	}
	defer body.Close()

	// Ensure that r.Body can be read again by other request handling routines.
	r.Body = ioutil.NopCloser(bytes.NewBuffer(data))

	return fmt.Sprintf("%s#%s", r.URL.String(), string(data))
}

func getWPTFYIGithubBot(ctx context.Context) (*github.Client, error) {
	client, err := shared.GetGithubClientFromToken(ctx, "github-wpt-fyi-bot-token")
	if err != nil {
		return nil, err
	}

	return client, nil
}
