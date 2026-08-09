// Plaid Link integration, used to connect the business bank account so
// incoming payments (e.g. Zelle deposits) can be tracked.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/plaid/plaid-go/v45/plaid"
)

// loadPlaidConfig reads Plaid credentials from the environment.
func loadPlaidConfig() (clientID, secret, env string, err error) {
	clientID = os.Getenv("PLAID_CLIENT_ID")
	secret = os.Getenv("PLAID_SECRET")
	env = os.Getenv("PLAID_ENV")
	if env == "" {
		env = "sandbox"
	}
	if clientID == "" || secret == "" {
		return "", "", "", fmt.Errorf("plaid is not configured: set PLAID_CLIENT_ID and PLAID_SECRET in .env")
	}
	return clientID, secret, env, nil
}

// newPlaidClient builds a Plaid API client for the configured environment.
func newPlaidClient() (*plaid.APIClient, error) {
	clientID, secret, env, err := loadPlaidConfig()
	if err != nil {
		return nil, err
	}

	plaidEnv := plaid.Sandbox
	if env == "production" {
		plaidEnv = plaid.Production
	}

	configuration := plaid.NewConfiguration()
	configuration.AddDefaultHeader("PLAID-CLIENT-ID", clientID)
	configuration.AddDefaultHeader("PLAID-SECRET", secret)
	configuration.UseEnvironment(plaidEnv)
	return plaid.NewAPIClient(configuration), nil
}

// plaidLinkTokenHandler requests a link_token from Plaid so the frontend can
// initialize Plaid Link and let the admin connect the business bank account.
func plaidLinkTokenHandler(w http.ResponseWriter, r *http.Request) {
	client, err := newPlaidClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	user := plaid.LinkTokenCreateRequestUser{
		ClientUserId: currentUsername(r),
	}
	request := plaid.NewLinkTokenCreateRequest(
		"Electroclima Pro Services",
		"en",
		[]plaid.CountryCode{plaid.COUNTRYCODE_US},
	)
	request.SetUser(user)
	request.SetProducts([]plaid.Products{plaid.PRODUCTS_TRANSACTIONS})

	resp, _, err := client.PlaidApi.LinkTokenCreate(context.Background()).LinkTokenCreateRequest(*request).Execute()
	if err != nil {
		plaidErr, convErr := plaid.ToPlaidError(err)
		if convErr == nil {
			http.Error(w, plaidErr.ErrorMessage, http.StatusBadGateway)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"link_token": resp.GetLinkToken()})
}

// plaidExchangeHandler swaps a Plaid Link public_token for a permanent
// access_token and stores it, completing the bank connection.
func plaidExchangeHandler(w http.ResponseWriter, r *http.Request) {
	publicToken := r.FormValue("public_token")
	if publicToken == "" {
		http.Error(w, "missing public_token", http.StatusBadRequest)
		return
	}
	institutionName := r.FormValue("institution_name")

	client, err := newPlaidClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exchangeResp, _, err := client.PlaidApi.ItemPublicTokenExchange(context.Background()).ItemPublicTokenExchangeRequest(
		*plaid.NewItemPublicTokenExchangeRequest(publicToken),
	).Execute()
	if err != nil {
		plaidErr, convErr := plaid.ToPlaidError(err)
		if convErr == nil {
			http.Error(w, plaidErr.ErrorMessage, http.StatusBadGateway)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if err := savePlaidItem(exchangeResp.GetItemId(), exchangeResp.GetAccessToken(), institutionName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// PlaidBankPageView is the template data for the "Bank Accounts" admin page.
type PlaidBankPageView struct {
	Items       []PlaidItem
	CSRFToken   string
	IsAdmin     bool
	Username    string
	DisplayName string
}

func plaidBankPageHandler(w http.ResponseWriter, r *http.Request) {
	items, err := listPlaidItems()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := PlaidBankPageView{
		Items:       items,
		CSRFToken:   currentCSRFToken(r),
		IsAdmin:     isAdmin(r),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
	}
	if err := mustTemplate("bank.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// plaidDeleteHandler disconnects a stored bank account.
func plaidDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := deletePlaidItem(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/bank", http.StatusSeeOther)
}
