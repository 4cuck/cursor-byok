package claudeaccount

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Account statuses.
const (
	// StatusOK means nothing is known to be wrong with the token.
	StatusOK = "ok"
	// StatusExpired means the token's own expiry time has passed.
	StatusExpired = "expired"
	// StatusRejected means Anthropic refused the token the last time it was
	// used here.
	StatusRejected = "rejected"
)

// Account is what the UI is shown of a saved account. It holds no token.
type Account struct {
	ID               string `json:"id"`
	Email            string `json:"email,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	// Plan is "pro", "max", "team" or "enterprise", or "" when it is not known.
	Plan          string `json:"plan,omitempty"`
	BillingType   string `json:"billingType,omitempty"`
	RateLimitTier string `json:"rateLimitTier,omitempty"`
	// UsageCredits is whether the account can pay for use beyond the plan's
	// limits, or nil when Anthropic has not said.
	UsageCredits *UsageCredits `json:"usageCredits,omitempty"`
	UsageWindows []UsageWindow `json:"usageWindows,omitempty"`
	Scopes       []string      `json:"scopes,omitempty"`
	// TokenHint is the start and the last characters of the token, enough to
	// tell two tokens apart and not enough to use one.
	TokenHint string `json:"tokenHint"`
	// ExpiresAtUnixMS is when the token expires, or 0 when that is not known.
	ExpiresAtUnixMS int64 `json:"expiresAtUnixMs,omitempty"`
	// Status is one of the Status constants.
	Status          string `json:"status"`
	CreatedAtUnixMS int64  `json:"createdAtUnixMs,omitempty"`
	UpdatedAtUnixMS int64  `json:"updatedAtUnixMs,omitempty"`
	// UsageAtUnixMS is when UsageWindows was last read from Anthropic, or 0
	// when it never was.
	UsageAtUnixMS int64 `json:"usageAtUnixMs,omitempty"`
	// DetailsError is why the last attempt to read the profile and usage did
	// not get both, as "code: detail", or "". CodeDetailsUnavailable in it
	// means the token works but may not read them, which is not a fault.
	DetailsError string `json:"detailsError,omitempty"`
}

// AccountToken pairs a saved account with its token, for Go callers that put
// tokens into model rows. It must not be sent to the UI.
type AccountToken struct {
	AccountID string
	Token     string
	// Usable is whether the account's status is StatusOK.
	Usable bool
}

// ImportResult is the outcome of Manager.Import.
type ImportResult struct {
	Account Account
	// Created is whether the credential was for an account that was not saved
	// yet. When false, the credential replaced the one of a saved account.
	Created bool
}

// Manager keeps the saved accounts. Its methods may be called from several
// goroutines.
//
// Lock order: a callback passed to Import or Remove runs while the manager is
// locked, so the callback may take other locks, and code that holds another
// lock must not call the manager.
type Manager struct {
	mu    sync.Mutex
	store *store
	api   *API
	now   func() time.Time
}

// NewManager returns a manager that stores accounts in dataRoot and reads
// account details through api.
func NewManager(dataRoot string, api *API) *Manager {
	return &Manager{
		store: &store{path: filepath.Join(dataRoot, storeFileName)},
		api:   api,
		now:   time.Now,
	}
}

// List returns the saved accounts, oldest first.
func (m *Manager) List() ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.store.load()
	if err != nil {
		return nil, err
	}
	now := m.now()
	accounts := make([]Account, 0, len(records))
	for _, item := range records {
		accounts = append(accounts, accountOf(item, now))
	}
	return accounts, nil
}

// Tokens returns the token of every saved account, oldest account first.
func (m *Manager) Tokens() ([]AccountToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.store.load()
	if err != nil {
		return nil, err
	}
	now := m.now()
	tokens := make([]AccountToken, 0, len(records))
	for _, item := range records {
		tokens = append(tokens, AccountToken{
			AccountID: item.ID,
			Token:     item.AccessToken,
			Usable:    statusOf(item, now) == StatusOK,
		})
	}
	return tokens, nil
}

// Import saves the credential in text, which is what ParseCredentials reads.
// The token is checked with Anthropic first: an expired or refused token is
// not saved. A token that works but may not read the account's profile is
// saved without one; such a token is its own account, because nothing says
// whose it is.
//
// A credential for an account that is already saved replaces that account's
// token; any other credential is saved as a new account. Two credentials are
// for the same account when Anthropic reports the same account and
// organization for both.
//
// beforeSave, when not nil, runs after the token is checked and before
// anything is stored. It gets the id of the account, the token the account had
// until now ("" for a new account) and its new token. An error from it
// abandons the import.
func (m *Manager) Import(ctx context.Context, text string, beforeSave func(accountID, previousToken, token string) error) (ImportResult, error) {
	credentials, err := ParseCredentials(text)
	if err != nil {
		return ImportResult{}, err
	}
	now := m.now()
	if credentials.ExpiresAtUnixMS > 0 && credentials.ExpiresAtUnixMS <= now.UnixMilli() {
		return ImportResult{}, codedError(CodeTokenExpired, "this token expired on "+time.UnixMilli(credentials.ExpiresAtUnixMS).UTC().Format("2006-01-02"))
	}

	// Anthropic is asked before the lock is taken: the answer can take seconds.
	found, err := m.inspect(ctx, credentials.AccessToken)
	if err != nil {
		return ImportResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.store.load()
	if err != nil {
		return ImportResult{}, err
	}
	index := findRecord(records, found.profile, credentials.AccessToken)
	created := index < 0
	var next record
	previousToken := ""
	if created {
		next = record{ID: uuid.NewString(), CreatedAtUnixMS: now.UnixMilli()}
	} else {
		next = records[index]
		previousToken = next.AccessToken
	}
	next.AccessToken = credentials.AccessToken
	next.ExpiresAtUnixMS = credentials.ExpiresAtUnixMS
	next.Scopes = credentials.Scopes
	next.SubscriptionType = credentials.SubscriptionType
	next.RateLimitTier = credentials.RateLimitTier
	next.UpdatedAtUnixMS = now.UnixMilli()
	next.Rejected = false
	next.DetailsError = found.problem
	if found.profile != nil {
		next.Profile = found.profile
	}
	if found.usage != nil {
		next.Usage = found.usage
		next.UsageAtUnixMS = now.UnixMilli()
	}

	if beforeSave != nil {
		if err := beforeSave(next.ID, previousToken, next.AccessToken); err != nil {
			return ImportResult{}, err
		}
	}
	if created {
		records = append(records, next)
	} else {
		records[index] = next
	}
	if err := m.store.save(records); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Account: accountOf(next, now), Created: created}, nil
}

// Refresh reads the profile and usage of the account again and saves what it
// learns. A failure to read them is reported in the returned account's
// DetailsError and Status, not as an error: the account is still there, and
// what was known before is kept.
func (m *Manager) Refresh(ctx context.Context, accountID string) (Account, error) {
	m.mu.Lock()
	records, err := m.store.load()
	if err != nil {
		m.mu.Unlock()
		return Account{}, err
	}
	index := indexOfID(records, accountID)
	if index < 0 {
		m.mu.Unlock()
		return Account{}, codedError(CodeNotFound, "this Claude account is no longer saved")
	}
	token := records[index].AccessToken
	m.mu.Unlock()

	found, inspectErr := m.inspect(ctx, token)

	m.mu.Lock()
	defer m.mu.Unlock()
	records, err = m.store.load()
	if err != nil {
		return Account{}, err
	}
	index = indexOfID(records, accountID)
	if index < 0 {
		return Account{}, codedError(CodeNotFound, "this Claude account is no longer saved")
	}
	now := m.now()
	next := records[index]
	if next.AccessToken != token {
		// The credential was replaced while Anthropic was being asked; what
		// came back describes the old token.
		return accountOf(next, now), nil
	}
	switch {
	case inspectErr == nil:
		next.Rejected = false
		next.DetailsError = found.problem
		if found.profile != nil {
			next.Profile = found.profile
		}
		if found.usage != nil {
			next.Usage = found.usage
			next.UsageAtUnixMS = now.UnixMilli()
		}
	case hasCode(inspectErr, CodeTokenRejected):
		next.Rejected = true
		next.DetailsError = inspectErr.Error()
	default:
		// Whether the token still works was not found out, so the account
		// keeps the standing it had.
		next.DetailsError = inspectErr.Error()
	}
	records[index] = next
	if err := m.store.save(records); err != nil {
		return Account{}, err
	}
	return accountOf(next, now), nil
}

// details is what Anthropic reports about the account behind a token.
type details struct {
	// profile is nil when it could not be read.
	profile *Profile
	// usage is nil when it could not be read.
	usage *Usage
	// problem is why the profile or the usage is missing, as "code: detail",
	// or "" when both were read.
	problem string
}

// inspect asks Anthropic about the account behind token.
//
// The error has CodeTokenRejected when Anthropic refuses the token, and
// CodeUnreachable when it could not be found out whether it does. A token
// that Anthropic accepts but that may not read the profile or the usage gives
// no error: what could not be read is nil, and details.problem says why.
func (m *Manager) inspect(ctx context.Context, token string) (details, error) {
	var found details
	profile, err := m.api.Profile(ctx, token)
	switch status := httpStatus(err); {
	case err == nil:
		found.profile = &profile
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Either Anthropic refuses the token, or the token may not read the
		// profile: the Claude CLI's long-lived tokens are for sending requests
		// and can lack that right. The model list is asked for as a second
		// opinion. When it is given, the token works and only the profile is
		// out of its reach.
		if _, err := m.api.Models(ctx, token); err != nil {
			if status := httpStatus(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
				return details{}, codedError(CodeTokenRejected, "Anthropic refused this token")
			}
			return details{}, wrapCoded(CodeUnreachable, "the token could not be checked with Anthropic", err)
		}
		found.problem = CodeDetailsUnavailable + ": this token may not read the account's profile"
	default:
		return details{}, wrapCoded(CodeUnreachable, "the token could not be checked with Anthropic", err)
	}

	usage, err := m.api.Usage(ctx, token)
	switch status := httpStatus(err); {
	case err == nil:
		found.usage = &usage
	case found.problem != "":
		// The profile could not be read either, and that is already reported.
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		found.problem = CodeDetailsUnavailable + ": this token may not read the account's usage"
	default:
		found.problem = CodeUnreachable + ": the usage could not be read: " + err.Error()
	}
	return found, nil
}

// hasCode reports whether err is an error of this package with the given code.
func hasCode(err error, code string) bool {
	return err != nil && strings.HasPrefix(err.Error(), code+":")
}

// Remove deletes the account. beforeRemove, when not nil, runs first with the
// account's token; an error from it leaves the account saved.
func (m *Manager) Remove(accountID string, beforeRemove func(token string) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.store.load()
	if err != nil {
		return err
	}
	index := indexOfID(records, accountID)
	if index < 0 {
		return codedError(CodeNotFound, "this Claude account is no longer saved")
	}
	if beforeRemove != nil {
		if err := beforeRemove(records[index].AccessToken); err != nil {
			return err
		}
	}
	records = append(records[:index:index], records[index+1:]...)
	return m.store.save(records)
}

// Models lists the models the saved accounts can use. It asks with the first
// account whose token is usable and moves on to the next when one fails.
func (m *Manager) Models(ctx context.Context) ([]Model, error) {
	tokens, err := m.Tokens()
	if err != nil {
		return nil, err
	}
	var firstErr error
	asked := false
	for _, item := range tokens {
		if !item.Usable {
			continue
		}
		asked = true
		models, err := m.api.Models(ctx, item.Token)
		if err == nil {
			return models, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if !asked {
		return nil, codedError(CodeNoUsableAccount, "no saved Claude account has a usable token")
	}
	if httpStatus(firstErr) == http.StatusUnauthorized {
		return nil, codedError(CodeTokenRejected, "Anthropic refused the token")
	}
	return nil, wrapCoded(CodeModelsFailed, "the model list could not be read", firstErr)
}

// findRecord returns the index of the saved account a credential belongs to,
// or -1 when it belongs to none. With a profile, that is the account Anthropic
// reports the same account and organization for; a record with the same token
// always matches.
func findRecord(records []record, profile *Profile, token string) int {
	for index, item := range records {
		if item.AccessToken == token {
			return index
		}
	}
	if profile == nil || profile.AccountUUID == "" {
		return -1
	}
	for index, item := range records {
		if item.Profile != nil && item.Profile.AccountUUID == profile.AccountUUID && item.Profile.OrganizationUUID == profile.OrganizationUUID {
			return index
		}
	}
	return -1
}

func indexOfID(records []record, accountID string) int {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return -1
	}
	for index, item := range records {
		if item.ID == accountID {
			return index
		}
	}
	return -1
}

func statusOf(item record, now time.Time) string {
	switch {
	case item.ExpiresAtUnixMS > 0 && item.ExpiresAtUnixMS <= now.UnixMilli():
		return StatusExpired
	case item.Rejected:
		return StatusRejected
	default:
		return StatusOK
	}
}

// accountOf returns what the UI is shown of item.
func accountOf(item record, now time.Time) Account {
	account := Account{
		ID:              item.ID,
		RateLimitTier:   item.RateLimitTier,
		Plan:            normalizePlan(item.SubscriptionType),
		Scopes:          append([]string(nil), item.Scopes...),
		TokenHint:       tokenHint(item.AccessToken),
		ExpiresAtUnixMS: item.ExpiresAtUnixMS,
		Status:          statusOf(item, now),
		CreatedAtUnixMS: item.CreatedAtUnixMS,
		UpdatedAtUnixMS: item.UpdatedAtUnixMS,
		UsageAtUnixMS:   item.UsageAtUnixMS,
		DetailsError:    item.DetailsError,
	}
	if profile := item.Profile; profile != nil {
		account.Email = profile.Email
		account.DisplayName = profile.DisplayName
		account.OrganizationName = profile.OrganizationName
		account.BillingType = profile.BillingType
		if plan := planOfOrganizationType(profile.OrganizationType); plan != "" {
			account.Plan = plan
		}
		if profile.RateLimitTier != "" {
			account.RateLimitTier = profile.RateLimitTier
		}
		if profile.UsageCreditsEnabled != nil {
			account.UsageCredits = &UsageCredits{Enabled: *profile.UsageCreditsEnabled}
		}
	}
	if usage := item.Usage; usage != nil {
		account.UsageWindows = append([]UsageWindow(nil), usage.Windows...)
		if usage.UsageCredits != nil {
			credits := *usage.UsageCredits
			account.UsageCredits = &credits
		}
	}
	return account
}

// planOfOrganizationType turns Anthropic's organization type into a plan name,
// the way the Claude CLI does. Types that are not a subscription give "".
func planOfOrganizationType(organizationType string) string {
	switch strings.ToLower(strings.TrimSpace(organizationType)) {
	case "claude_pro":
		return "pro"
	case "claude_max":
		return "max"
	case "claude_team":
		return "team"
	case "claude_enterprise":
		return "enterprise"
	default:
		return ""
	}
}

// normalizePlan keeps the plan names this package knows and drops any other.
func normalizePlan(subscriptionType string) string {
	switch plan := strings.ToLower(strings.TrimSpace(subscriptionType)); plan {
	case "pro", "max", "team", "enterprise":
		return plan
	default:
		return ""
	}
}

// tokenHint returns the token's prefix and its last four characters.
func tokenHint(token string) string {
	const prefixLength = len("sk-ant-oat01-")
	if len(token) < prefixLength+12 {
		return "…"
	}
	return token[:prefixLength] + "…" + token[len(token)-4:]
}

// httpStatus returns the status of the StatusError in err's chain, or 0.
func httpStatus(err error) int {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status
	}
	return 0
}
