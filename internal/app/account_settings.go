package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	userSettingsEndpoint    = "https://chatgpt.com/backend-api/settings/user"
	accountSettingsMaxBytes = 1 << 20
	accountSettingsEndpoint = "https://chatgpt.com/backend-api/settings/account_user_setting"
	accountSettingsTimeout  = 30 * time.Second
)

func trainingSettingNotice() string {
	return "Adding this account checks model training and turns it off if needed."
}

func connectAccount(ctx context.Context, client *http.Client, tokens tokenResponse) (*Account, error) {
	account := accountFromState(accountState{
		IDToken:      tokens.IDToken,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		LastRefresh:  time.Now(),
	})
	if err := disableTraining(ctx, client, account); err != nil {
		return nil, err
	}
	return account, nil
}

func disableTraining(ctx context.Context, client *http.Client, account *Account) error {
	account.mu.Lock()
	token := account.AccessToken
	accountID := claimsFromToken(account.IDToken).Auth.AccountID
	account.mu.Unlock()
	if token == "" || accountID == "" {
		return errors.New("disable training: account credentials are incomplete")
	}

	requestContext, cancel := context.WithTimeout(ctx, accountSettingsTimeout)
	defer cancel()
	// Read the account-scoped user setting before attempting a mutation. A missing
	// or unreadable setting is not proof that the account is already opted out.
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, userSettingsEndpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", accountID)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("read training setting: %w", err)
	}
	data, readErr := func() ([]byte, error) {
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return nil, fmt.Errorf("account settings returned %s", response.Status)
		}
		return io.ReadAll(io.LimitReader(response.Body, accountSettingsMaxBytes+1))
	}()
	if readErr != nil {
		return fmt.Errorf("read training setting: %w", readErr)
	}
	if len(data) > accountSettingsMaxBytes {
		return errors.New("read training setting: response too large")
	}
	var settings struct {
		Settings struct {
			TrainingAllowed *bool `json:"training_allowed"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("read training setting: invalid settings response: %w", err)
	}
	if settings.Settings.TrainingAllowed == nil {
		return errors.New("read training setting: settings.training_allowed is missing or null")
	}
	if !*settings.Settings.TrainingAllowed {
		return nil
	}

	endpoint, err := url.Parse(accountSettingsEndpoint)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("feature", "training_allowed")
	query.Set("value", "false")
	endpoint.RawQuery = query.Encode()
	request, err = http.NewRequestWithContext(requestContext, http.MethodPatch, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", accountID)
	response, err = client.Do(request)
	if err != nil {
		return fmt.Errorf("disable training: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("disable training: account settings returned %s", response.Status)
	}
	return nil
}
