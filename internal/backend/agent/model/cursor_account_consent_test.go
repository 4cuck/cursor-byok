package modeladapter

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

type consentTestBackend struct {
	mu               sync.Mutex
	frames           map[string]chan *agentv1.AgentClientMessage
	streams          []cursorAccountTestStream
	userConsents     []*aiserverv1.SetUserNoZdrModelConsentRequest
	userAuths        []string
	teamConsents     []*aiserverv1.SetTeamMemberNoZdrModelConsentRequest
	clientActions    []*aiserverv1.ClientActionRequest
	getTeamsCalls    int
	onUserConsent    func(auth string, req *aiserverv1.SetUserNoZdrModelConsentRequest) (int, proto.Message)
	onGetTeams       func(auth string) (int, proto.Message)
	onTeamConsent    func(auth string, req *aiserverv1.SetTeamMemberNoZdrModelConsentRequest) (int, proto.Message)
	onClientAction   func(auth string, req *aiserverv1.ClientActionRequest) (int, proto.Message)
}

func (b *consentTestBackend) channel(requestID string) chan *agentv1.AgentClientMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.frames[requestID]
	if !ok {
		ch = make(chan *agentv1.AgentClientMessage, 16)
		b.frames[requestID] = ch
	}
	return ch
}

func startConsentTestBackend(t *testing.T, play func(stream int, auth string, w http.ResponseWriter, flusher http.Flusher)) *consentTestBackend {
	t.Helper()
	b := &consentTestBackend{frames: map[string]chan *agentv1.AgentClientMessage{}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id := &aiserverv1.BidiRequestId{}
		if r.URL.Path != cursorAccountRunPath || len(body) < 5 || proto.Unmarshal(body[5:], id) != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		var run *agentv1.AgentClientMessage
		select {
		case run = <-b.channel(id.GetRequestId()):
		case <-time.After(4 * time.Second):
			return
		}
		auth := r.Header.Get("authorization")
		b.mu.Lock()
		b.streams = append(b.streams, cursorAccountTestStream{authorization: auth, run: run})
		stream := len(b.streams)
		b.mu.Unlock()
		play(stream, auth, w, flusher)
	}))
	t.Cleanup(agent.Close)

	writeProto := func(w http.ResponseWriter, status int, msg proto.Message) {
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if msg != nil {
			raw, _ := proto.Marshal(msg)
			_, _ = w.Write(raw)
		}
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("authorization")
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			writeProto(w, http.StatusOK, &aiserverv1.GetServerConfigResponse{
				AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL},
			})
		case cursorAccountBidiAppendPath:
			request := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			raw, err := hex.DecodeString(request.GetData())
			message := &agentv1.AgentClientMessage{}
			if err != nil || proto.Unmarshal(raw, message) != nil {
				http.Error(w, "bad frame", http.StatusBadRequest)
				return
			}
			b.channel(request.GetRequestId().GetRequestId()) <- message
			w.WriteHeader(http.StatusOK)
		case cursorAccountSetUserNoZdrConsentPath:
			req := &aiserverv1.SetUserNoZdrModelConsentRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b.mu.Lock()
			b.userConsents = append(b.userConsents, req)
			b.userAuths = append(b.userAuths, auth)
			handler := b.onUserConsent
			b.mu.Unlock()
			if handler != nil {
				status, resp := handler(auth, req)
				writeProto(w, status, resp)
				return
			}
			writeProto(w, http.StatusOK, &aiserverv1.SetUserNoZdrModelConsentResponse{Consented: true})
		case cursorAccountGetTeamsPath:
			b.mu.Lock()
			b.getTeamsCalls++
			handler := b.onGetTeams
			b.mu.Unlock()
			if handler != nil {
				status, resp := handler(auth)
				writeProto(w, status, resp)
				return
			}
			writeProto(w, http.StatusOK, &aiserverv1.GetTeamsResponse{})
		case cursorAccountSetTeamMemberNoZdrConsentPath:
			req := &aiserverv1.SetTeamMemberNoZdrModelConsentRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b.mu.Lock()
			b.teamConsents = append(b.teamConsents, req)
			handler := b.onTeamConsent
			b.mu.Unlock()
			if handler != nil {
				status, resp := handler(auth, req)
				writeProto(w, status, resp)
				return
			}
			writeProto(w, http.StatusUnauthorized, nil)
		case cursorAccountClientActionPath:
			req := &aiserverv1.ClientActionRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b.mu.Lock()
			b.clientActions = append(b.clientActions, req)
			handler := b.onClientAction
			b.mu.Unlock()
			if handler != nil {
				status, resp := handler(auth, req)
				writeProto(w, status, resp)
				return
			}
			writeProto(w, http.StatusOK, &aiserverv1.ClientActionResponse{Success: false})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })
	return b
}

func makeConnectErrorFrame(t *testing.T, errDetails *aiserverv1.ErrorDetails) string {
	t.Helper()
	rawProto, err := proto.Marshal(errDetails)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"error": map[string]any{
			"code":    "permission_denied",
			"message": "Error",
			"details": []map[string]any{
				{
					"type":  "aiserver.v1.ErrorDetails",
					"value": base64.RawStdEncoding.EncodeToString(rawProto),
					"debug": map[string]any{
						"details": map[string]any{
							"title":  errDetails.GetDetails().GetTitle(),
							"detail": errDetails.GetDetails().GetDetail(),
						},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestCursorAccountNeedsDataRetentionConsent(t *testing.T) {
	shouldMatch := []string{
		"cursor refused the model call: You must acknowledge Claude Fable 5's data retention policy to use the model.",
		"cursor refused the model call: Please accept the data-retention terms before using Claude Fable 5.2.",
		"cursor refused the model call: no-ZDR model consent required",
	}
	for _, msg := range shouldMatch {
		err := errors.New(msg)
		if !cursorAccountNeedsDataRetentionConsent(err) {
			t.Errorf("cursorAccountNeedsDataRetentionConsent(%q) = false, want true", msg)
		}
		if cursorAccountShouldResetSavedState(err) {
			t.Errorf("cursorAccountShouldResetSavedState(%q) = true, want false (must preserve checkpoint)", msg)
		}
		if CursorAccountQuotaRefusalText(msg) {
			t.Errorf("CursorAccountQuotaRefusalText(%q) = true, want false (must not set quota cooldown)", msg)
		}
		if !cursorAccountRefusesModelText(msg) {
			t.Errorf("cursorAccountRefusesModelText(%q) = false, want true (must allow account failover if unresolved)", msg)
		}
	}

	shouldNotMatch := []string{
		"cursor refused the model call: Switched to Auto after reaching Other Models usage limit.",
		"cursor refused the model call: Model name is not valid: \"claude-fable-5-2\"",
		"cursor refused the model call: If you are logged in, try logging out and back in.",
		"Too many computers have been used with this account",
		"upstream unavailable",
	}
	for _, msg := range shouldNotMatch {
		if cursorAccountNeedsDataRetentionConsent(errors.New(msg)) {
			t.Errorf("cursorAccountNeedsDataRetentionConsent(%q) = true, want false", msg)
		}
	}
}

func TestCursorNoZdrConsentTarget(t *testing.T) {
	cases := []struct {
		modelID     string
		errText     string
		wantModel   string
		wantVersion string
	}{
		{"claude-fable-5", "", "claude-fable-5", "fable-data-retention-v1"},
		{"claude-fable-5-1", "", "claude-fable-5", "fable-data-retention-v1"},
		{"claude-fable-5-2", "", "claude-fable-5", "fable-data-retention-v1"},
		{"claude-fable-5-2-low-fast", "", "claude-fable-5", "fable-data-retention-v1"},
		{"claude-fable-5-thinking-high", "", "claude-fable-5", "fable-data-retention-v1"},
		{"cursor_account1:claude-fable-5-2[context=1m,effort=high]", "", "claude-fable-5", "fable-data-retention-v1"},
		{"default", "You must acknowledge Claude Fable 5's data retention policy to use the model.", "claude-fable-5", "fable-data-retention-v1"},
		{"custom-no-zdr-model", "data retention consent required", "custom-no-zdr-model", ""},
	}
	for _, tc := range cases {
		gotModel, gotVersion := cursorNoZdrConsentTarget(tc.modelID, tc.errText)
		if gotModel != tc.wantModel || gotVersion != tc.wantVersion {
			t.Errorf("cursorNoZdrConsentTarget(%q, %q) = (%q, %q), want (%q, %q)",
				tc.modelID, tc.errText, gotModel, gotVersion, tc.wantModel, tc.wantVersion)
		}
	}
}

func TestExtractCursorDashboardActionsFiltersUnsafeActions(t *testing.T) {
	frame := makeConnectErrorFrame(t, &aiserverv1.ErrorDetails{
		Details: &aiserverv1.CustomErrorDetails{
			Title:  "Data Retention Policy",
			Detail: "You must acknowledge Claude Fable 5's data retention policy to use the model.",
			Buttons: []*aiserverv1.ErrorButton{
				{
					Label: "Upgrade",
					Action: &aiserverv1.ErrorButton_Upgrade{
						Upgrade: &aiserverv1.UpgradeAction{
							DashboardAction: &aiserverv1.DashboardAction{Action: "upgrade_plan"},
						},
					},
				},
				{
					Label: "Buy Credits",
					Action: &aiserverv1.ErrorButton_DashboardAction{
						DashboardAction: &aiserverv1.DashboardAction{Action: "purchase_credits_billing"},
					},
				},
				{
					Label: "Disable Privacy Mode",
					Action: &aiserverv1.ErrorButton_DashboardAction{
						DashboardAction: &aiserverv1.DashboardAction{Action: "disable_privacy_mode"},
					},
				},
				{
					Label: "Review Data Policy",
					Action: &aiserverv1.ErrorButton_DashboardAction{
						DashboardAction: &aiserverv1.DashboardAction{
							Action: "accept_no_zdr_model_consent",
							Args:   map[string]string{"modelId": "claude-fable-5"},
						},
					},
				},
			},
		},
	})

	actions := extractCursorDashboardActions([]byte(frame))
	if len(actions) != 1 {
		t.Fatalf("extracted %d actions, want 1: %+v", len(actions), actions)
	}
	if actions[0].GetAction() != "accept_no_zdr_model_consent" || actions[0].GetArgs()["modelId"] != "claude-fable-5" {
		t.Fatalf("unexpected action: %+v", actions[0])
	}
}

func TestCursorAccountGatewayAutoAcceptsFableUserConsentAndRetries(t *testing.T) {
	const conversationID = "conv-fable-auto-consent"
	t.Cleanup(func() { ClearCursorAccountState(conversationID) })
	cursorAccountRememberState(conversationID, &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("keep-this-turn")}})

	var backend *consentTestBackend
	backend = startConsentTestBackend(t, func(stream int, _ string, w http.ResponseWriter, flusher http.Flusher) {
		if stream == 1 {
			writeCursorAgentEnd(w, `{"error":{"code":"permission_denied","message":"Error","details":[{"type":"aiserver.v1.ErrorDetails","debug":{"details":{"title":"Server Error","detail":"You must acknowledge Claude Fable 5's data retention policy to use the model."}}}]}}`)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "hello from fable")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})

	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test-token"})
	text, err := streamCursorAccountText(gateway, StreamRequest{
		ModelID:         "cursor_account1:claude-fable-5-2",
		ProviderModelID: "claude-fable-5-2",
		ConversationID:  conversationID,
		Messages:        []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if text != "hello from fable" {
		t.Fatalf("text = %q, want %q", text, "hello from fable")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.streams) != 2 {
		t.Fatalf("streams = %d, want 2 (refusal + retry)", len(backend.streams))
	}
	if len(backend.userConsents) != 1 {
		t.Fatalf("userConsents = %d, want 1", len(backend.userConsents))
	}
	req := backend.userConsents[0]
	if req.GetModelId() != "claude-fable-5" || !req.GetEnabled() || !req.GetAcknowledged() || req.GetConsentVersion() != "fable-data-retention-v1" {
		t.Fatalf("unexpected SetUserNoZdrModelConsentRequest: %+v", req)
	}
	if backend.getTeamsCalls != 0 {
		t.Fatalf("GetTeams called %d times, want 0 when user consent succeeds", backend.getTeamsCalls)
	}
	if !HasCursorAccountState(conversationID) {
		t.Fatal("saved conversation checkpoint was cleared on data-retention refusal")
	}
}

func TestCursorAccountGatewayAutoAcceptsFableTeamMemberConsentFallback(t *testing.T) {
	consentedTeam := false
	backend := startConsentTestBackend(t, func(stream int, _ string, w http.ResponseWriter, flusher http.Flusher) {
		if !consentedTeam {
			writeCursorAgentEnd(w, `{"error":{"details":[{"debug":{"details":{"detail":"You must acknowledge Claude Fable 5's data retention policy to use the model."}}}]}}`)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "team member ok")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	backend.onUserConsent = func(_ string, _ *aiserverv1.SetUserNoZdrModelConsentRequest) (int, proto.Message) {
		// User-scope consent succeeds on personal scope, but active team still requires team-member consent.
		return http.StatusOK, &aiserverv1.SetUserNoZdrModelConsentResponse{Consented: true}
	}
	backend.onGetTeams = func(_ string) (int, proto.Message) {
		return http.StatusOK, &aiserverv1.GetTeamsResponse{
			Teams: []*aiserverv1.Team{{Id: 4242, Name: "Test Team"}},
		}
	}
	backend.onTeamConsent = func(_ string, req *aiserverv1.SetTeamMemberNoZdrModelConsentRequest) (int, proto.Message) {
		if req.GetTeamId() == 4242 && req.GetModelId() == "claude-fable-5" && req.GetEnabled() && req.GetAcknowledged() {
			consentedTeam = true
			return http.StatusOK, &aiserverv1.SetTeamMemberNoZdrModelConsentResponse{Consented: true}
		}
		return http.StatusBadRequest, nil
	}

	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer team-member-token"})
	text, err := streamCursorAccountText(gateway, StreamRequest{
		ModelID:         "claude-fable-5-2-low-fast",
		ProviderModelID: "claude-fable-5-2-low-fast",
		Messages:        []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if text != "team member ok" {
		t.Fatalf("text = %q, want %q", text, "team member ok")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.teamConsents) != 1 || backend.teamConsents[0].GetTeamId() != 4242 {
		t.Fatalf("teamConsents = %+v, want team 4242", backend.teamConsents)
	}
	if len(backend.streams) != 3 {
		t.Fatalf("streams = %d, want 3 (initial + after user consent + after team-member consent)", len(backend.streams))
	}
}

func TestCursorAccountGatewayAutoAcceptsFableViaDashboardClientActionFallback(t *testing.T) {
	consentedViaAction := false
	errFrame := makeConnectErrorFrame(t, &aiserverv1.ErrorDetails{
		Details: &aiserverv1.CustomErrorDetails{
			Title:  "Data Retention Policy",
			Detail: "You must acknowledge Claude Fable 5's data retention policy to use the model.",
			Buttons: []*aiserverv1.ErrorButton{
				{
					Label: "Review Data Policy",
					Action: &aiserverv1.ErrorButton_DashboardAction{
						DashboardAction: &aiserverv1.DashboardAction{
							Action: "review_no_zdr_model_consent",
							Args:   map[string]string{"modelId": "claude-fable-5"},
						},
					},
				},
			},
		},
	})

	backend := startConsentTestBackend(t, func(_ int, _ string, w http.ResponseWriter, flusher http.Flusher) {
		if !consentedViaAction {
			writeCursorAgentEnd(w, errFrame)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "client action ok")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	backend.onUserConsent = func(_ string, _ *aiserverv1.SetUserNoZdrModelConsentRequest) (int, proto.Message) {
		// Simulate Cursor bumping consent_version so the direct call fails.
		return http.StatusBadRequest, nil
	}
	backend.onClientAction = func(_ string, req *aiserverv1.ClientActionRequest) (int, proto.Message) {
		if req.GetAction() == "review_no_zdr_model_consent" {
			return http.StatusOK, &aiserverv1.ClientActionResponse{
				Success: false,
				Confirmation: &aiserverv1.ClientActionConfirmation{
					Title:        "Accept Data Retention",
					ConfirmLabel: "Accept",
					Action: &aiserverv1.DashboardAction{
						Action: "confirm_no_zdr_model_consent",
						Args:   map[string]string{"modelId": "claude-fable-5", "version": "fable-data-retention-v2"},
					},
				},
			}
		}
		if req.GetAction() == "confirm_no_zdr_model_consent" {
			consentedViaAction = true
			return http.StatusOK, &aiserverv1.ClientActionResponse{Success: true}
		}
		return http.StatusOK, &aiserverv1.ClientActionResponse{Success: false}
	}

	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	text, err := streamCursorAccountText(gateway, StreamRequest{
		ModelID:         "claude-fable-5-2",
		ProviderModelID: "claude-fable-5-2",
		Messages:        []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if text != "client action ok" {
		t.Fatalf("text = %q, want %q", text, "client action ok")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.clientActions) != 2 {
		t.Fatalf("clientActions = %d, want 2 (review + confirm)", len(backend.clientActions))
	}
	if backend.clientActions[0].GetAction() != "review_no_zdr_model_consent" ||
		backend.clientActions[1].GetAction() != "confirm_no_zdr_model_consent" {
		t.Fatalf("unexpected clientActions sequence: %+v", backend.clientActions)
	}
}

type multiAccountConsentAuth struct {
	mu           sync.Mutex
	current      string
	candidates   []string
	tokens       map[string]string
	apiKeys      map[string]bool
	refreshedTok map[string]string
}

func (a *multiAccountConsentAuth) Authorization(_ context.Context) (string, error) {
	return a.AuthorizationFor(context.Background(), a.current)
}

func (a *multiAccountConsentAuth) AuthorizationFor(_ context.Context, accountID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if accountID == "" {
		accountID = a.current
	}
	return a.tokens[accountID], nil
}

func (a *multiAccountConsentAuth) AccountIsLocked(_ string) bool { return false }
func (a *multiAccountConsentAuth) CurrentAccountID() string      { return a.current }
func (a *multiAccountConsentAuth) CandidateAccountIDs() []string { return a.candidates }
func (a *multiAccountConsentAuth) UsesAPIKey(accountID string) bool {
	return a.apiKeys[accountID]
}
func (a *multiAccountConsentAuth) ForceRefresh(_ context.Context, accountID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if tok := a.refreshedTok[accountID]; tok != "" {
		a.tokens[accountID] = tok
		return tok, nil
	}
	return "", errors.New("no refresh token")
}
func (a *multiAccountConsentAuth) MarkDeviceLocked(_ string) error { return nil }

func TestCursorAccountGatewayFailsOverWhenTeamAdminConsentRequired(t *testing.T) {
	t.Cleanup(func() { ClearAllCursorAccountExhausted("") })
	ClearAllCursorAccountExhausted("")

	backend := startConsentTestBackend(t, func(_ int, auth string, w http.ResponseWriter, flusher http.Flusher) {
		if auth == "Bearer acct-enterprise" {
			writeCursorAgentEnd(w, `{"error":{"details":[{"debug":{"details":{"detail":"You must acknowledge Claude Fable 5's data retention policy to use the model."}}}]}}`)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "served by acct-personal")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	backend.onUserConsent = func(auth string, _ *aiserverv1.SetUserNoZdrModelConsentRequest) (int, proto.Message) {
		if auth == "Bearer acct-enterprise" {
			return http.StatusForbidden, nil
		}
		return http.StatusOK, &aiserverv1.SetUserNoZdrModelConsentResponse{Consented: true}
	}
	backend.onGetTeams = func(_ string) (int, proto.Message) {
		return http.StatusOK, &aiserverv1.GetTeamsResponse{
			Teams: []*aiserverv1.Team{{Id: 99, Name: "Enterprise Org"}},
		}
	}
	backend.onTeamConsent = func(_ string, _ *aiserverv1.SetTeamMemberNoZdrModelConsentRequest) (int, proto.Message) {
		return http.StatusUnauthorized, nil
	}

	auth := &multiAccountConsentAuth{
		current:    "acct-enterprise",
		candidates: []string{"acct-enterprise", "acct-personal"},
		tokens: map[string]string{
			"acct-enterprise": "Bearer acct-enterprise",
			"acct-personal":   "Bearer acct-personal",
		},
	}

	text, err := streamCursorAccountText(newCursorAccountGateway(auth), StreamRequest{
		ModelID:         "claude-fable-5-2",
		ProviderModelID: "claude-fable-5-2",
		CursorAccountID: "acct-enterprise",
		Messages:        []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if text != "served by acct-personal" {
		t.Fatalf("text = %q, want %q", text, "served by acct-personal")
	}
	if CursorAccountModelExhausted("acct-enterprise", "claude-fable-5-2") {
		t.Fatal("data-retention refusal must not mark account as quota-exhausted")
	}
}

func TestCursorAccountGatewayRefreshesAPIKeyThenAcceptsFableConsent(t *testing.T) {
	consented := false
	backend := startConsentTestBackend(t, func(_ int, auth string, w http.ResponseWriter, flusher http.Flusher) {
		if auth == "Bearer stale-api-key-jwt" {
			writeCursorAgentEnd(w, `{"error":{"details":[{"debug":{"details":{"detail":"If you are logged in, try logging out and back in."}}}]}}`)
			flusher.Flush()
			return
		}
		if !consented {
			writeCursorAgentEnd(w, `{"error":{"details":[{"debug":{"details":{"detail":"You must acknowledge Claude Fable 5's data retention policy to use the model."}}}]}}`)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "refreshed and consented")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	backend.onUserConsent = func(auth string, _ *aiserverv1.SetUserNoZdrModelConsentRequest) (int, proto.Message) {
		if auth != "Bearer fresh-api-key-jwt" {
			return http.StatusUnauthorized, nil
		}
		consented = true
		return http.StatusOK, &aiserverv1.SetUserNoZdrModelConsentResponse{Consented: true}
	}

	auth := &multiAccountConsentAuth{
		current:      "acct-apikey",
		candidates:   []string{"acct-apikey"},
		tokens:       map[string]string{"acct-apikey": "Bearer stale-api-key-jwt"},
		apiKeys:      map[string]bool{"acct-apikey": true},
		refreshedTok: map[string]string{"acct-apikey": "Bearer fresh-api-key-jwt"},
	}

	text, err := streamCursorAccountText(newCursorAccountGateway(auth), StreamRequest{
		ModelID:         "claude-fable-5-2",
		ProviderModelID: "claude-fable-5-2",
		CursorAccountID: "acct-apikey",
		Messages:        []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if text != "refreshed and consented" {
		t.Fatalf("text = %q, want %q", text, "refreshed and consented")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.userAuths) != 1 || backend.userAuths[0] != "Bearer fresh-api-key-jwt" {
		t.Fatalf("userConsent called with auths=%v, want refreshed token", backend.userAuths)
	}
	if !strings.EqualFold(backend.streams[len(backend.streams)-1].authorization, "Bearer fresh-api-key-jwt") {
		t.Fatalf("final stream did not use refreshed token: %q", backend.streams[len(backend.streams)-1].authorization)
	}
}
