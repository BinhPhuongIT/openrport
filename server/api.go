package chserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudradar-monitoring/rport/server/validation"

	"github.com/cloudradar-monitoring/rport/share/query"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/tomasen/realip"
	"golang.org/x/crypto/ssh"

	"github.com/cloudradar-monitoring/rport/server/api"
	errors2 "github.com/cloudradar-monitoring/rport/server/api/errors"
	"github.com/cloudradar-monitoring/rport/server/api/jobs"
	"github.com/cloudradar-monitoring/rport/server/api/middleware"
	"github.com/cloudradar-monitoring/rport/server/api/users"
	"github.com/cloudradar-monitoring/rport/server/cgroups"
	"github.com/cloudradar-monitoring/rport/server/clients"
	"github.com/cloudradar-monitoring/rport/server/clientsauth"
	"github.com/cloudradar-monitoring/rport/server/ports"
	"github.com/cloudradar-monitoring/rport/server/script"
	"github.com/cloudradar-monitoring/rport/server/vault"
	chshare "github.com/cloudradar-monitoring/rport/share"
	"github.com/cloudradar-monitoring/rport/share/comm"
	"github.com/cloudradar-monitoring/rport/share/enums"
	"github.com/cloudradar-monitoring/rport/share/models"
	"github.com/cloudradar-monitoring/rport/share/random"
	"github.com/cloudradar-monitoring/rport/share/security"
	"github.com/cloudradar-monitoring/rport/share/ws"
)

const (
	queryParamSort = "sort"

	routeParamClientID      = "client_id"
	routeParamUserID        = "user_id"
	routeParamJobID         = "job_id"
	routeParamGroupID       = "group_id"
	routeParamVaultValueID  = "vault_value_id"
	routeParamScriptValueID = "script_value_id"

	ErrCodeMissingRouteVar = "ERR_CODE_MISSING_ROUTE_VAR"
	ErrCodeInvalidRequest  = "ERR_CODE_INVALID_REQUEST"
	ErrCodeAlreadyExist    = "ERR_CODE_ALREADY_EXIST"

	minVersionScriptExecSupport = "0.1.35"
)

var generateNewJobID = func() (string, error) {
	return random.UUID4()
}

var apiUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type JobProvider interface {
	GetByJID(clientID, jid string) (*models.Job, error)
	GetSummariesByClientID(clientID string) ([]*models.JobSummary, error)
	GetByMultiJobID(jid string) ([]*models.Job, error)
	// SaveJob creates or updates a job
	SaveJob(job *models.Job) error
	// CreateJob creates a new job. If already exist with a given JID - do nothing and return nil
	CreateJob(job *models.Job) error
	GetMultiJob(jid string) (*models.MultiJob, error)
	GetAllMultiJobSummaries() ([]*models.MultiJobSummary, error)
	SaveMultiJob(multiJob *models.MultiJob) error
	Close() error
}

func (al *APIListener) wrapWithAuthMiddleware(f http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorized, username, err := al.lookupUser(r)
		if err != nil {
			if errors.Is(err, ErrTooManyRequests) {
				al.jsonErrorResponse(w, http.StatusTooManyRequests, err)
				return
			}
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		if !al.handleBannedIPs(w, r, authorized) {
			return
		}

		if !authorized || username == "" {
			al.bannedUsers.Add(username)
			al.jsonErrorResponse(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}

		newCtx := api.WithUser(r.Context(), username)
		f.ServeHTTP(w, r.WithContext(newCtx))
	}
}

func (al *APIListener) wrapClientAccessMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if al.insecureForTests {
			next.ServeHTTP(w, r)
			return
		}

		vars := mux.Vars(r)
		clientID := vars[routeParamClientID]
		if clientID == "" {
			al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
			return
		}

		curUser, err := al.getUserModelForAuth(r.Context())
		if err != nil {
			al.jsonError(w, err)
			return
		}

		err = al.clientService.CheckClientAccess(clientID, curUser)
		if err != nil {
			al.jsonError(w, err)
			return
		}

		next.ServeHTTP(w, r)
	}
}

func (al *APIListener) handleBannedIPs(w http.ResponseWriter, r *http.Request, authorized bool) (ok bool) {
	if al.bannedIPs != nil {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, fmt.Errorf("failed to split host port for %q: %v", r.RemoteAddr, err))
			return false
		}

		if authorized {
			al.bannedIPs.AddSuccessAttempt(ip)
		} else {
			al.bannedIPs.AddBadAttempt(ip)
		}
	}

	return true
}

func (al *APIListener) initRouter() {
	r := mux.NewRouter()
	sub := r.PathPrefix("/api/v1").Subrouter()
	sub.HandleFunc("/status", al.handleGetStatus).Methods(http.MethodGet)
	sub.HandleFunc("/me", al.handleGetMe).Methods(http.MethodGet)
	sub.HandleFunc("/me", al.handleChangeMe).Methods(http.MethodPut)
	sub.HandleFunc("/me/ip", al.handleGetIP).Methods(http.MethodGet)
	sub.HandleFunc("/me/token", al.handlePostToken).Methods(http.MethodPost)
	sub.HandleFunc("/me/token", al.handleDeleteToken).Methods(http.MethodDelete)
	sub.HandleFunc("/clients", al.handleGetClients).Methods(http.MethodGet)
	sub.HandleFunc("/clients/{client_id}", al.wrapClientAccessMiddleware(al.handleGetClient)).Methods(http.MethodGet)
	sub.HandleFunc("/clients/{client_id}", al.wrapClientAccessMiddleware(al.handleDeleteClient)).Methods(http.MethodDelete)
	sub.HandleFunc("/clients/{client_id}/acl", al.wrapAdminAccessMiddleware(al.handlePostClientACL)).Methods(http.MethodPost)
	sub.HandleFunc("/clients/{client_id}/tunnels", al.wrapClientAccessMiddleware(al.handlePutClientTunnel)).Methods(http.MethodPut)
	sub.HandleFunc("/clients/{client_id}/tunnels/{tunnel_id}", al.wrapClientAccessMiddleware(al.handleDeleteClientTunnel)).Methods(http.MethodDelete)
	sub.HandleFunc("/clients/{client_id}/commands", al.wrapClientAccessMiddleware(al.handlePostCommand)).Methods(http.MethodPost)
	sub.HandleFunc("/clients/{client_id}/commands", al.wrapClientAccessMiddleware(al.handleGetCommands)).Methods(http.MethodGet)
	sub.HandleFunc("/clients/{client_id}/commands/{job_id}", al.wrapClientAccessMiddleware(al.handleGetCommand)).Methods(http.MethodGet)
	sub.HandleFunc("/clients/{client_id}/scripts", al.wrapClientAccessMiddleware(al.handleExecuteScript)).Methods(http.MethodPost)
	sub.HandleFunc("/clients/{client_id}/updates-status", al.wrapClientAccessMiddleware(al.handleRefreshUpdatesStatus)).Methods(http.MethodPost)
	sub.HandleFunc("/client-groups", al.handleGetClientGroups).Methods(http.MethodGet)
	sub.HandleFunc("/client-groups", al.wrapAdminAccessMiddleware(al.handlePostClientGroups)).Methods(http.MethodPost)
	sub.HandleFunc("/client-groups/{group_id}", al.wrapAdminAccessMiddleware(al.handlePutClientGroup)).Methods(http.MethodPut)
	sub.HandleFunc("/client-groups/{group_id}", al.handleGetClientGroup).Methods(http.MethodGet)
	sub.HandleFunc("/client-groups/{group_id}", al.wrapAdminAccessMiddleware(al.handleDeleteClientGroup)).Methods(http.MethodDelete)
	sub.HandleFunc("/users", al.wrapStaticPassModeMiddleware(al.wrapAdminAccessMiddleware(al.handleGetUsers))).Methods(http.MethodGet)
	sub.HandleFunc("/users", al.wrapStaticPassModeMiddleware(al.wrapAdminAccessMiddleware(al.handleChangeUser))).Methods(http.MethodPost)
	sub.HandleFunc("/users/{user_id}", al.wrapStaticPassModeMiddleware(al.wrapAdminAccessMiddleware(al.handleChangeUser))).Methods(http.MethodPut)
	sub.HandleFunc("/users/{user_id}", al.wrapStaticPassModeMiddleware(al.wrapAdminAccessMiddleware(al.handleDeleteUser))).Methods(http.MethodDelete)
	sub.HandleFunc("/commands", al.handlePostMultiClientCommand).Methods(http.MethodPost)
	sub.HandleFunc("/commands", al.handleGetMultiClientCommands).Methods(http.MethodGet)
	sub.HandleFunc("/commands/{job_id}", al.handleGetMultiClientCommand).Methods(http.MethodGet)
	sub.HandleFunc("/clients-auth", al.wrapAdminAccessMiddleware(al.handleGetClientsAuth)).Methods(http.MethodGet)
	sub.HandleFunc("/clients-auth", al.wrapAdminAccessMiddleware(al.handlePostClientsAuth)).Methods(http.MethodPost)
	sub.HandleFunc("/clients-auth/{client_auth_id}", al.wrapAdminAccessMiddleware(al.handleDeleteClientAuth)).Methods(http.MethodDelete)
	sub.HandleFunc("/vault-admin", al.handleGetVaultStatus).Methods(http.MethodGet)
	sub.HandleFunc("/vault-admin/sesame", al.wrapAdminAccessMiddleware(al.handleVaultUnlock)).Methods(http.MethodPost)
	sub.HandleFunc("/vault-admin/init", al.wrapAdminAccessMiddleware(al.handleVaultInit)).Methods(http.MethodPost)
	sub.HandleFunc("/vault-admin/sesame", al.wrapAdminAccessMiddleware(al.handleVaultLock)).Methods(http.MethodDelete)
	sub.HandleFunc("/vault", al.handleListVaultValues).Methods(http.MethodGet)
	sub.HandleFunc("/vault", al.handleVaultStoreValue).Methods(http.MethodPost)
	sub.HandleFunc("/vault/{"+routeParamVaultValueID+"}", al.handleReadVaultValue).Methods(http.MethodGet)
	sub.HandleFunc("/vault/{"+routeParamVaultValueID+"}", al.handleVaultStoreValue).Methods(http.MethodPut)
	sub.HandleFunc("/vault/{"+routeParamVaultValueID+"}", al.handleVaultDeleteValue).Methods(http.MethodDelete)
	sub.HandleFunc("/library/scripts", al.handleListScripts).Methods(http.MethodGet)
	sub.HandleFunc("/library/scripts", al.handleScriptCreate).Methods(http.MethodPost)
	sub.HandleFunc("/library/scripts/{"+routeParamScriptValueID+"}", al.handleScriptUpdate).Methods(http.MethodPut)
	sub.HandleFunc("/library/scripts/{"+routeParamScriptValueID+"}", al.handleReadScript).Methods(http.MethodGet)
	sub.HandleFunc("/library/scripts/{"+routeParamScriptValueID+"}", al.handleDeleteScript).Methods(http.MethodDelete)
	sub.HandleFunc("/scripts", al.handlePostMultiClientScript).Methods(http.MethodPost)

	// add authorization middleware
	if !al.insecureForTests {
		_ = sub.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
			route.HandlerFunc(al.wrapWithAuthMiddleware(route.GetHandler()))
			return nil
		})
	}

	// all routes defined below do not have authorization middleware, auth is done in each handlers separately
	sub.HandleFunc("/login", al.handleGetLogin).Methods(http.MethodGet)
	sub.HandleFunc("/login", al.handlePostLogin).Methods(http.MethodPost)
	sub.HandleFunc("/logout", al.handleDeleteLogout).Methods(http.MethodDelete)
	sub.HandleFunc("/verify-2fa", al.handlePostVerify2FAToken).Methods(http.MethodPost)

	// web sockets
	// common auth middleware is not used due to JS issue https://stackoverflow.com/questions/22383089/is-it-possible-to-use-bearer-authentication-for-websocket-upgrade-requests
	sub.HandleFunc("/ws/commands", al.wsAuth(http.HandlerFunc(al.handleCommandsWS))).Methods(http.MethodGet)
	sub.HandleFunc("/ws/scripts", al.wsAuth(http.HandlerFunc(al.handleScriptsWS))).Methods(http.MethodGet)

	if al.config.Server.EnableWsTestEndpoints {
		sub.HandleFunc("/test/commands/ui", al.wsCommands)
		sub.HandleFunc("/test/scripts/ui", al.wsScripts)
	}

	if al.bannedIPs != nil {
		// add middleware to reject banned IPs
		_ = sub.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
			route.HandlerFunc(security.RejectBannedIPs(route.GetHandler(), al.bannedIPs))
			return nil
		})
	}

	// add max bytes middleware
	_ = sub.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		route.HandlerFunc(middleware.MaxBytes(route.GetHandler(), al.config.Server.MaxRequestBytes))
		return nil
	})

	al.router = r
}

func (al *APIListener) writeJSONResponse(w http.ResponseWriter, statusCode int, response interface{}) {
	b, err := json.Marshal(response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(statusCode)
	if _, err := w.Write(b); err != nil {
		al.Errorf("error writing response: %s", err)
	}
}

func (al *APIListener) jsonErrorResponse(w http.ResponseWriter, statusCode int, err error) {
	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromError(err, "", ""))
}

func (al *APIListener) jsonError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	errCode := ""
	var apiErr errors2.APIError
	var apiErrs errors2.APIErrors
	switch {
	case errors.As(err, &apiErr):
		statusCode = apiErr.HTTPStatus
		errCode = apiErr.ErrCode
	case errors.As(err, &apiErrs):
		if len(apiErrs) > 0 {
			statusCode = apiErrs[0].HTTPStatus
			errCode = apiErrs[0].ErrCode
		}
	}

	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromError(err, errCode, ""))
}

func (al *APIListener) jsonErrorResponseWithErrCode(w http.ResponseWriter, statusCode int, errCode, title string) {
	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromMessage(errCode, title, ""))
}

func (al *APIListener) jsonErrorResponseWithTitle(w http.ResponseWriter, statusCode int, title string) {
	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromMessage("", title, ""))
}

func (al *APIListener) jsonErrorResponseWithDetail(w http.ResponseWriter, statusCode int, errCode, title, detail string) {
	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromMessage(errCode, title, detail))
}

func (al *APIListener) jsonErrorResponseWithError(w http.ResponseWriter, statusCode int, title string, err error) {
	var detail string
	if err != nil {
		detail = err.Error()
	}
	al.writeJSONResponse(w, statusCode, api.NewErrAPIPayloadFromMessage("", title, detail))
}

type twoFAResponse struct {
	SendTo         string `json:"send_to"`
	DeliveryMethod string `json:"delivery_method"`
}

type loginResponse struct {
	Token *string        `json:"token"`  // null if 2fa is on
	TwoFA *twoFAResponse `json:"two_fa"` // null if 2fa is off
}

func (al *APIListener) handleGetLogin(w http.ResponseWriter, req *http.Request) {
	basicUser, basicPwd, basicAuthProvided := req.BasicAuth()
	if !basicAuthProvided {
		// TODO: consider to move this check from all API endpoints to middleware similar to https://github.com/cloudradar-monitoring/rport/pull/199/commits/4ca1ca9f56c557762d79a60ffc96d2de47f3133c
		// ban IP if it sends a lot of bad requests
		if !al.handleBannedIPs(w, req, false) {
			return
		}
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "basic auth is required")
		return
	}

	al.handleLogin(basicUser, basicPwd, w, req)
}

func (al *APIListener) handleLogin(username, pwd string, w http.ResponseWriter, req *http.Request) {
	if al.bannedUsers.IsBanned(username) {
		al.jsonErrorResponseWithTitle(w, http.StatusTooManyRequests, ErrTooManyRequests.Error())
		return
	}

	if username == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "username is required")
		return
	}

	authorized, err := al.validateCredentials(username, pwd)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if !al.handleBannedIPs(w, req, authorized) {
		return
	}

	if !authorized {
		al.bannedUsers.Add(username)
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if al.config.API.IsTwoFAOn() {
		sendTo, err := al.twoFASrv.SendToken(req.Context(), username)
		if err != nil {
			al.jsonError(w, err)
			return
		}

		al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(loginResponse{
			TwoFA: &twoFAResponse{
				SendTo:         sendTo,
				DeliveryMethod: al.twoFASrv.MsgSrv.DeliveryMethod(),
			},
		}))
		return
	}

	al.sendJWTToken(username, w, req)
}

func (al *APIListener) sendJWTToken(username string, w http.ResponseWriter, req *http.Request) {
	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	tokenStr, err := al.createAuthToken(lifetime, username)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	response := api.NewSuccessPayload(loginResponse{
		Token: &tokenStr,
	})
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handlePostLogin(w http.ResponseWriter, req *http.Request) {
	username, pwd, err := parseLoginPostRequestBody(req)
	if err != nil {
		// ban IP if it sends a lot of bad requests
		if !al.handleBannedIPs(w, req, false) {
			return
		}
		al.jsonError(w, err)
		return
	}

	al.handleLogin(username, pwd, w, req)
}

func parseLoginPostRequestBody(req *http.Request) (string, string, error) {
	reqContentType := req.Header.Get("Content-Type")
	if reqContentType == "application/x-www-form-urlencoded" {
		err := req.ParseForm()
		if err != nil {
			return "", "", errors2.APIError{
				Err:        fmt.Errorf("failed to parse form: %v", err),
				HTTPStatus: http.StatusBadRequest,
			}
		}
		return req.PostForm.Get("username"), req.PostForm.Get("password"), nil
	}
	if reqContentType == "application/json" {
		type loginReq struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		var params loginReq
		err := parseRequestBody(req.Body, &params)
		if err != nil {
			return "", "", err
		}
		return params.Username, params.Password, nil
	}
	return "", "", errors2.APIError{
		Message:    fmt.Sprintf("unsupported content type: %s", reqContentType),
		HTTPStatus: http.StatusBadRequest,
	}
}

func parseTokenLifetime(req *http.Request) (time.Duration, error) {
	lifetimeStr := req.URL.Query().Get("token-lifetime")
	if lifetimeStr == "" {
		lifetimeStr = "0"
	}
	lifetime, err := strconv.ParseInt(lifetimeStr, 10, 0)
	if err != nil {
		return 0, fmt.Errorf("invalid token-lifetime : %s", err)
	}
	result := time.Duration(lifetime) * time.Second
	if result > maxTokenLifetime {
		return 0, fmt.Errorf("requested token lifetime exceeds max allowed %d", maxTokenLifetime/time.Second)
	}
	if result <= 0 {
		result = defaultTokenLifetime
	}
	return result, nil
}

func (al *APIListener) handleDeleteLogout(w http.ResponseWriter, req *http.Request) {
	token, tokenProvided := getBearerToken(req)
	if token == "" || !tokenProvided {
		// ban IP if it sends a lot of bad requests
		if !al.handleBannedIPs(w, req, false) {
			return
		}
		al.jsonErrorResponse(w, http.StatusBadRequest, fmt.Errorf("authorization Bearer token required"))
		return
	}

	valid, user, apiSession, err := al.validateBearerToken(token)
	if err != nil {
		if errors.Is(err, ErrTooManyRequests) {
			al.jsonErrorResponse(w, http.StatusTooManyRequests, err)
			return
		}
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if !al.handleBannedIPs(w, req, valid) {
		return
	}
	if !valid {
		al.bannedUsers.Add(user)
		al.jsonErrorResponse(w, http.StatusBadRequest, fmt.Errorf("token is invalid or expired"))
		return
	}

	err = al.apiSessionRepo.Delete(apiSession)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handlePostVerify2FAToken(w http.ResponseWriter, req *http.Request) {
	username, err := al.parseAndValidate2FATokenRequest(req)
	if err != nil {
		if !al.handleBannedIPs(w, req, false) {
			return
		}
		al.jsonError(w, err)
		return
	}

	al.sendJWTToken(username, w, req)
}

func (al *APIListener) parseAndValidate2FATokenRequest(req *http.Request) (username string, err error) {
	if !al.config.API.IsTwoFAOn() {
		return "", errors2.APIError{
			HTTPStatus: http.StatusConflict,
			Message:    "2fa is disabled",
		}
	}

	var reqBody struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	err = parseRequestBody(req.Body, &reqBody)
	if err != nil {
		return "", err
	}

	if al.bannedUsers.IsBanned(reqBody.Username) {
		return reqBody.Username, errors2.APIError{
			HTTPStatus: http.StatusTooManyRequests,
			Err:        ErrTooManyRequests,
		}
	}

	if reqBody.Username == "" {
		return "", errors2.APIError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "username is required",
		}
	}

	if reqBody.Token == "" {
		return reqBody.Username, errors2.APIError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "token is required",
		}
	}

	return reqBody.Username, al.twoFASrv.ValidateToken(reqBody.Username, reqBody.Token)
}

func (al *APIListener) handleGetStatus(w http.ResponseWriter, req *http.Request) {
	countActive, err := al.clientService.CountActive()
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	countDisconnected, err := al.clientService.CountDisconnected()
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	var twoFADelivery string
	if al.twoFASrv.MsgSrv != nil {
		twoFADelivery = al.twoFASrv.MsgSrv.DeliveryMethod()
	}

	response := api.NewSuccessPayload(map[string]interface{}{
		"version":                chshare.BuildVersion,
		"clients_connected":      countActive,
		"clients_disconnected":   countDisconnected,
		"fingerprint":            al.fingerprint,
		"connect_url":            al.config.Server.URL,
		"clients_auth_source":    al.clientAuthProvider.Source(),
		"clients_auth_mode":      al.getClientsAuthMode(),
		"users_auth_source":      al.usersService.GetProviderType(),
		"two_fa_enabled":         al.config.API.IsTwoFAOn(),
		"two_fa_delivery_method": twoFADelivery,
	})

	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleGetClients(w http.ResponseWriter, req *http.Request) {
	var err error
	sortFunc, desc, err := getCorrespondingSortFunc(req.URL.Query().Get(queryParamSort))
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	filterOptions := query.ExtractFilterOptions(req)
	filterErr := query.ValidateFilterOptions(filterOptions, clientsSupportedFields)
	if filterErr != nil {
		al.jsonError(w, filterErr)
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	cls, err := al.clientService.GetUserClients(curUser, filterOptions)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	sortFunc(cls, desc)

	clientsPayload := convertToClientsPayload(cls)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(clientsPayload))
}

func (al *APIListener) handleGetClient(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	clientID := vars[routeParamClientID]

	client, err := al.clientService.GetByID(clientID)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("client with id %q not found", clientID))
		return
	}

	clientPayload := convertToClientPayload(client)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(clientPayload))
}

type UserPayload struct {
	Username    string   `json:"username"`
	Groups      []string `json:"groups"`
	TwoFASendTo string   `json:"two_fa_send_to"`
}

func (al *APIListener) handleGetUsers(w http.ResponseWriter, req *http.Request) {
	usrs, err := al.usersService.GetAll()
	if err != nil {
		al.jsonError(w, err)
		return
	}

	usersToSend := make([]UserPayload, 0, len(usrs))
	for i := range usrs {
		user := usrs[i]
		usersToSend = append(usersToSend, UserPayload{
			Username:    user.Username,
			Groups:      user.Groups,
			TwoFASendTo: user.TwoFASendTo,
		})
	}

	response := api.NewSuccessPayload(usersToSend)
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleChangeUser(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routeParamUserID]
	if !userIDExists {
		userID = ""
	}

	var user users.User
	err := parseRequestBody(req.Body, &user)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if err := al.usersService.Change(&user, userID); err != nil {
		al.jsonError(w, err)
		return
	}

	if userIDExists {
		al.Debugf("User [%s] updated.", userID)
		w.WriteHeader(http.StatusNoContent)
	} else {
		al.Debugf("User [%s] created.", user.Username)
		w.WriteHeader(http.StatusCreated)
	}
}

func (al *APIListener) handleDeleteUser(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routeParamUserID]
	if !userIDExists {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Empty user id provided")
		return
	}

	if err := al.usersService.Delete(userID); err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	al.Debugf("User [%s] deleted.", userID)
}

type ClientPayload struct {
	ID                     string                  `json:"id"`
	Name                   string                  `json:"name"`
	Address                string                  `json:"address"`
	Hostname               string                  `json:"hostname"`
	OS                     string                  `json:"os"`
	OSFullName             string                  `json:"os_full_name"`
	OSVersion              string                  `json:"os_version"`
	OSArch                 string                  `json:"os_arch"`
	OSFamily               string                  `json:"os_family"`
	OSKernel               string                  `json:"os_kernel"`
	OSVirtualizationSystem string                  `json:"os_virtualization_system"`
	OSVirtualizationRole   string                  `json:"os_virtualization_role"`
	NumCPUs                int                     `json:"num_cpus"`
	CPUFamily              string                  `json:"cpu_family"`
	CPUModel               string                  `json:"cpu_model"`
	CPUModelName           string                  `json:"cpu_model_name"`
	CPUVendor              string                  `json:"cpu_vendor"`
	MemoryTotal            uint64                  `json:"mem_total"`
	Timezone               string                  `json:"timezone"`
	ClientAuthID           string                  `json:"client_auth_id"`
	Version                string                  `json:"version"`
	DisconnectedAt         *time.Time              `json:"disconnected_at"`
	ConnectionState        clients.ConnectionState `json:"connection_state"`
	IPv4                   []string                `json:"ipv4"`
	IPv6                   []string                `json:"ipv6"`
	Tags                   []string                `json:"tags"`
	AllowedUserGroups      []string                `json:"allowed_user_groups"`
	Tunnels                []*clients.Tunnel       `json:"tunnels"`
	UpdatesStatus          *models.UpdatesStatus   `json:"updates_status"`
}

func convertToClientsPayload(clients []*clients.Client) []ClientPayload {
	r := make([]ClientPayload, 0, len(clients))
	for _, cur := range clients {
		r = append(r, convertToClientPayload(cur))
	}
	return r
}

func convertToClientPayload(client *clients.Client) ClientPayload {
	return ClientPayload{
		ID:                     client.ID,
		Name:                   client.Name,
		OS:                     client.OS,
		OSArch:                 client.OSArch,
		OSFamily:               client.OSFamily,
		OSKernel:               client.OSKernel,
		Hostname:               client.Hostname,
		IPv4:                   client.IPv4,
		IPv6:                   client.IPv6,
		Tags:                   client.Tags,
		Version:                client.Version,
		Address:                client.Address,
		Tunnels:                client.Tunnels,
		DisconnectedAt:         client.DisconnectedAt,
		ConnectionState:        client.ConnectionState(),
		ClientAuthID:           client.ClientAuthID,
		OSFullName:             client.OSFullName,
		OSVersion:              client.OSVersion,
		OSVirtualizationSystem: client.OSVirtualizationSystem,
		OSVirtualizationRole:   client.OSVirtualizationRole,
		CPUFamily:              client.CPUFamily,
		CPUModel:               client.CPUModel,
		CPUModelName:           client.CPUModelName,
		CPUVendor:              client.CPUVendor,
		Timezone:               client.Timezone,
		NumCPUs:                client.NumCPUs,
		MemoryTotal:            client.MemoryTotal,
		AllowedUserGroups:      client.AllowedUserGroups,
		UpdatesStatus:          client.UpdatesStatus,
	}
}

func getCorrespondingSortFunc(sortStr string) (sortFunc func(a []*clients.Client, desc bool), desc bool, err error) {
	var sortField string
	if strings.HasPrefix(sortStr, "-") {
		desc = true
		sortField = sortStr[1:]
	} else {
		sortField = sortStr
	}

	switch sortField {
	case "":
		sortFunc = clients.SortByID
	case "id":
		sortFunc = clients.SortByID
	case "name":
		sortFunc = clients.SortByName
	case "os":
		sortFunc = clients.SortByOS
	case "hostname":
		sortFunc = clients.SortByHostname
	case "version":
		sortFunc = clients.SortByVersion
	default:
		err = fmt.Errorf("incorrect format of %q query param", queryParamSort)
	}
	return
}

func (al *APIListener) handleDeleteClient(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	clientID := vars[routeParamClientID]
	err := al.clientService.DeleteOffline(clientID)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	al.Debugf("Client %q deleted.", clientID)
}

type clientACLRequest struct {
	AllowedUserGroups []string `json:"allowed_user_groups"`
}

func (al *APIListener) handlePostClientACL(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routeParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
		return
	}

	var reqBody clientACLRequest
	err := parseRequestBody(req.Body, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.usersService.ExistGroups(reqBody.AllowedUserGroups)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.clientService.SetACL(cid, reqBody.AllowedUserGroups)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

const (
	URISchemeMaxLength = 15

	idleTimeoutMinutesQueryParam = "idle-timeout-minutes"
	skipIdleTimeoutQueryParam    = "skip-idle-timeout"

	ErrCodeLocalPortInUse        = "ERR_CODE_LOCAL_PORT_IN_USE"
	ErrCodeRemotePortNotOpen     = "ERR_CODE_REMOTE_PORT_NOT_OPEN"
	ErrCodeTunnelExist           = "ERR_CODE_TUNNEL_EXIST"
	ErrCodeTunnelToPortExist     = "ERR_CODE_TUNNEL_TO_PORT_EXIST"
	ErrCodeURISchemeLengthExceed = "ERR_CODE_URI_SCHEME_LENGTH_EXCEED"
	ErrCodeInvalidACL            = "ERR_CODE_INVALID_ACL"
)

func (al *APIListener) handlePutClientTunnel(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	clientID := vars[routeParamClientID]
	if clientID == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "client id is missing")
		return
	}

	client, err := al.clientService.GetActiveByID(clientID)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("client with id %s not found", clientID))
		return
	}

	localAddr := req.URL.Query().Get("local")
	remoteAddr := req.URL.Query().Get("remote")
	remoteStr := localAddr + ":" + remoteAddr
	if localAddr == "" {
		remoteStr = remoteAddr
	}
	remote, err := chshare.DecodeRemote(remoteStr)
	if err != nil {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("failed to decode %q: %v", remoteStr, err))
		return
	}

	idleTimeoutMinutesStr := req.URL.Query().Get(idleTimeoutMinutesQueryParam)
	skipIdleTimeout, err := strconv.ParseBool(req.URL.Query().Get(skipIdleTimeoutQueryParam))
	if err != nil {
		skipIdleTimeout = false
	}

	idleTimeout, err := validation.ResolveIdleTunnelTimeoutValue(idleTimeoutMinutesStr, skipIdleTimeout)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	remote.IdleTimeoutMinutes = int(idleTimeout.Minutes())

	aclStr := req.URL.Query().Get("acl")
	if _, err = clients.ParseTunnelACL(aclStr); err != nil {
		al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeInvalidACL, fmt.Sprintf("Invalid ACL: %s", err))
		return
	}
	if aclStr != "" {
		remote.ACL = &aclStr
	}

	schemeStr := req.URL.Query().Get("scheme")
	if len(schemeStr) > URISchemeMaxLength {
		al.jsonErrorResponseWithDetail(w, http.StatusBadRequest, ErrCodeURISchemeLengthExceed, "Invalid URI scheme.", "Exceeds the max length.")
		return
	}
	if schemeStr != "" {
		remote.Scheme = &schemeStr
	}

	if existing := client.FindTunnelByRemote(remote); existing != nil {
		al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeTunnelExist, "Tunnel already exist.")
		return
	}

	for _, t := range client.Tunnels {
		if t.Remote.Remote() == remote.Remote() && t.EqualACL(remote.ACL) {
			al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeTunnelToPortExist, fmt.Sprintf("Tunnel to port %s already exist.", remote.RemotePort))
			return
		}
	}

	if checkPortStr := req.URL.Query().Get("check_port"); checkPortStr != "0" {
		if !al.checkRemotePort(w, *remote, client.Connection) {
			return
		}
	}

	// make next steps thread-safe
	client.Lock()
	defer client.Unlock()

	if remote.IsLocalSpecified() && !al.checkLocalPort(w, remote.LocalPort) {
		return
	}

	tunnels, err := al.clientService.StartClientTunnels(client, []*chshare.Remote{remote})
	if err != nil {
		al.jsonError(w, err)
		return
	}
	response := api.NewSuccessPayload(tunnels[0])
	al.writeJSONResponse(w, http.StatusOK, response)
}

// TODO: remove this check, do it in client srv in startClientTunnels when https://github.com/cloudradar-monitoring/rport/pull/252 will be in master.
// APIError needs both httpStatusCode and errorCode. To avoid too many merge conflicts with PR252 temporarily use this check to avoid breaking UI
func (al *APIListener) checkLocalPort(w http.ResponseWriter, localPort string) bool {
	lport, err := strconv.Atoi(localPort)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid port: %s.", localPort), err)
		return false
	}

	busyPorts, err := ports.ListBusyPorts()
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return false
	}

	if busyPorts.Contains(lport) {
		al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeLocalPortInUse, fmt.Sprintf("Port %d already in use.", lport))
		return false
	}

	return true
}

func (al *APIListener) checkRemotePort(w http.ResponseWriter, remote chshare.Remote, conn ssh.Conn) bool {
	req := &comm.CheckPortRequest{
		HostPort: remote.Remote(),
		Timeout:  al.config.Server.CheckPortTimeout,
	}
	resp := &comm.CheckPortResponse{}
	err := comm.SendRequestAndGetResponse(conn, comm.RequestTypeCheckPort, req, resp)
	if err != nil {
		if _, ok := err.(*comm.ClientError); ok {
			al.jsonErrorResponse(w, http.StatusConflict, err)
		} else {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		}
		return false
	}

	if !resp.Open {
		al.jsonErrorResponseWithDetail(
			w,
			http.StatusBadRequest,
			ErrCodeRemotePortNotOpen,
			fmt.Sprintf("Port %s is not in listening state.", remote.RemotePort),
			resp.ErrMsg,
		)
		return false
	}

	return true
}

func (al *APIListener) handleDeleteClientTunnel(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	clientID := vars[routeParamClientID]
	if clientID == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "client id is missing")
		return
	}

	force := false
	forceStr := req.URL.Query().Get("force")
	if forceStr != "" {
		var err error
		force, err = strconv.ParseBool(forceStr)
		if err != nil {
			al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Invalid force param: %v.", forceStr))
			return
		}
	}

	client, err := al.clientService.GetActiveByID(clientID)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("client with id %s not found", clientID))
		return
	}

	tunnelID := vars["tunnel_id"]
	if tunnelID == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "tunnel id is missing")
		return
	}

	// make next steps thread-safe
	client.Lock()
	defer client.Unlock()

	tunnel := client.FindTunnel(tunnelID)
	if tunnel == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "tunnel not found")
		return
	}

	err = client.TerminateTunnel(tunnel, force)
	if err != nil {
		al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGetMe returns the currently logged in user and the groups the user belongs to.
func (al *APIListener) handleGetMe(w http.ResponseWriter, req *http.Request) {
	user, err := al.getUserModel(req.Context())
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	if user == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "user not found")
		return
	}

	me := UserPayload{
		Username:    user.Username,
		Groups:      user.Groups,
		TwoFASendTo: user.TwoFASendTo,
	}
	response := api.NewSuccessPayload(me)
	al.writeJSONResponse(w, http.StatusOK, response)
}

type changeMeRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	OldPassword string `json:"old_password"`
	TwoFASendTo string `json:"two_fa_send_to"`
}

func (al *APIListener) handleChangeMe(w http.ResponseWriter, req *http.Request) {
	var r changeMeRequest
	err := parseRequestBody(req.Body, &r)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if r.Password != "" {
		if r.OldPassword == "" {
			al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "Missing old password.")
			return
		}

		if !verifyPassword(curUser.Password, r.OldPassword) {
			al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "Incorrect old password.")
			return
		}
	}

	if err := al.usersService.Change(&users.User{
		Username:    r.Username,
		Password:    r.Password,
		TwoFASendTo: r.TwoFASendTo,
	}, curUser.Username); err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// TODO: remove
func (al *APIListener) getUserModel(ctx context.Context) (*users.User, error) {
	curUsername := api.GetUser(ctx, al.Logger)
	if curUsername == "" {
		return nil, nil
	}

	user, err := al.userSrv.GetByUsername(curUsername)
	if err != nil {
		return nil, err
	}

	return user, err
}

// TODO: move to userSrv
func (al *APIListener) getUserModelForAuth(ctx context.Context) (*users.User, error) {
	usr, err := al.getUserModel(ctx)
	if err != nil {
		return nil, errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusInternalServerError,
		}
	}

	if usr == nil {
		return nil, errors2.APIError{
			Message:    "unauthorized access",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	return usr, nil
}

func (al *APIListener) handleGetIP(w http.ResponseWriter, req *http.Request) {
	ipResp := struct {
		IP string `json:"ip"`
	}{
		IP: realip.FromRequest(req),
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(ipResp))
}

const (
	MinCredentialsLength = 3

	ErrCodeClientAuthSingleClient = "ERR_CODE_CLIENT_AUTH_SINGLE"
	ErrCodeClientAuthRO           = "ERR_CODE_CLIENT_AUTH_RO"

	ErrCodeClientAuthHasClient = "ERR_CODE_CLIENT_AUTH_HAS_CLIENT"
	ErrCodeClientAuthNotFound  = "ERR_CODE_CLIENT_AUTH_NOT_FOUND"
)

func (al *APIListener) handleGetClientsAuth(w http.ResponseWriter, req *http.Request) {
	rClients, err := al.clientAuthProvider.GetAll()
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	clientsauth.SortByID(rClients, false)

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(rClients))
}

func (al *APIListener) handlePostClientsAuth(w http.ResponseWriter, req *http.Request) {
	if !al.allowClientAuthWrite(w) {
		return
	}

	var newClient clientsauth.ClientAuth
	err := parseRequestBody(req.Body, &newClient)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if len(newClient.ID) < MinCredentialsLength {
		al.jsonErrorResponseWithDetail(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid or missing ID.", fmt.Sprintf("Min size is %d.", MinCredentialsLength))
		return
	}

	if len(newClient.Password) < MinCredentialsLength {
		al.jsonErrorResponseWithDetail(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid or missing password.", fmt.Sprintf("Min size is %d.", MinCredentialsLength))
		return
	}

	added, err := al.clientAuthProvider.Add(&newClient)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if !added {
		al.jsonErrorResponseWithDetail(w, http.StatusConflict, ErrCodeAlreadyExist, fmt.Sprintf("Client Auth with ID %q already exist.", newClient.ID), "")
		return
	}

	al.Infof("ClientAuth %q created.", newClient.ID)

	w.WriteHeader(http.StatusCreated)
}

func (al *APIListener) handleDeleteClientAuth(w http.ResponseWriter, req *http.Request) {
	if !al.allowClientAuthWrite(w) {
		return
	}

	vars := mux.Vars(req)
	clientAuthID := vars["client_auth_id"]
	if clientAuthID == "" {
		al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeMissingRouteVar, "Missing 'client_auth_id' route param.")
		return
	}

	force := false
	forceStr := req.URL.Query().Get("force")
	if forceStr != "" {
		var err error
		force, err = strconv.ParseBool(forceStr)
		if err != nil {
			al.jsonErrorResponseWithErrCode(w, http.StatusBadRequest, ErrCodeInvalidRequest, fmt.Sprintf("Invalid force param %v.", forceStr))
			return
		}
	}

	existing, err := al.clientAuthProvider.Get(clientAuthID)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if existing == nil {
		al.jsonErrorResponseWithErrCode(w, http.StatusNotFound, ErrCodeClientAuthNotFound, fmt.Sprintf("Client Auth with ID=%q not found.", clientAuthID))
		return
	}

	allClients := al.clientService.GetAllByClientID(clientAuthID)
	if !force && len(allClients) > 0 {
		al.jsonErrorResponseWithErrCode(w, http.StatusConflict, ErrCodeClientAuthHasClient, fmt.Sprintf("Client Auth expected to have no active or disconnected bound client(s), got %d.", len(allClients)))
		return
	}

	for _, s := range allClients {
		if err := al.clientService.ForceDelete(s); err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}
	}

	err = al.clientAuthProvider.Delete(clientAuthID)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	al.Infof("ClientAuth %q deleted.", clientAuthID)

	w.WriteHeader(http.StatusNoContent)
}

type clientsAuthMode string

const (
	clientsAuthModeRO = "Read Only"
	clientsAuthModeRW = "Read Write"
)

func (al *APIListener) getClientsAuthMode() clientsAuthMode {
	if al.isClientsAuthWriteable() {
		return clientsAuthModeRW
	}
	return clientsAuthModeRO
}

func (al *APIListener) isClientsAuthWriteable() bool {
	return al.clientAuthProvider.IsWriteable() && al.config.Server.AuthWrite
}

func (al *APIListener) allowClientAuthWrite(w http.ResponseWriter) bool {
	if !al.clientAuthProvider.IsWriteable() {
		al.jsonErrorResponseWithErrCode(w, http.StatusMethodNotAllowed, ErrCodeClientAuthSingleClient, "Client authentication is enabled only for a single user.")
		return false
	}

	if !al.config.Server.AuthWrite {
		al.jsonErrorResponseWithErrCode(w, http.StatusMethodNotAllowed, ErrCodeClientAuthRO, "Client authentication has been attached in read-only mode.")
		return false
	}

	return true
}

func (al *APIListener) handlePostCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routeParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
		return
	}

	execCmdInput := &api.ExecuteInput{}
	err := parseRequestBody(req.Body, &execCmdInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	execCmdInput.ClientID = cid
	execCmdInput.IsScript = false

	al.handleExecuteCommand(req.Context(), w, execCmdInput)
}

func (al *APIListener) handleExecuteCommand(ctx context.Context, w http.ResponseWriter, executeInput *api.ExecuteInput) {
	if executeInput.Command == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Command cannot be empty.")
		return
	}
	if err := validation.ValidateInterpreter(executeInput.Interpreter, executeInput.IsScript); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid interpreter.", err)
		return
	}

	if executeInput.TimeoutSec <= 0 {
		executeInput.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	client, err := al.clientService.GetActiveByID(executeInput.ClientID)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find an active client with id=%q.", executeInput.ClientID), err)
		return
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Active client with id=%q not found.", executeInput.ClientID))
		return
	}

	// send the command to the client
	// Send a job with all possible info in order to get the full-populated job back (in client-listener) when it's done.
	// Needed when server restarts to get all job data from client. Because on server restart job running info is lost.
	jid, err := generateNewJobID()
	if err != nil {
		al.jsonError(w, err)
		return
	}
	curJob := models.Job{
		JobSummary: models.JobSummary{
			JID:        jid,
			FinishedAt: nil,
		},
		ClientID:    executeInput.ClientID,
		ClientName:  client.Name,
		Command:     executeInput.Command,
		Interpreter: executeInput.Interpreter,
		CreatedBy:   api.GetUser(ctx, al.Logger),
		TimeoutSec:  executeInput.TimeoutSec,
		Result:      nil,
		Cwd:         executeInput.Cwd,
		IsSudo:      executeInput.IsSudo,
		IsScript:    executeInput.IsScript,
		HasShebang:  executeInput.HasShebang,
	}
	sshResp := &comm.RunCmdResponse{}
	err = comm.SendRequestAndGetResponse(client.Connection, comm.RequestTypeRunCmd, curJob, sshResp)
	if err != nil {
		if _, ok := err.(*comm.ClientError); ok {
			al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
		} else {
			al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to execute remote command.", err)
		}
		return
	}

	// set fields received in response
	curJob.PID = &sshResp.Pid
	curJob.StartedAt = sshResp.StartedAt
	curJob.Status = models.JobStatusRunning

	if err := al.jobProvider.CreateJob(&curJob); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist a new job.", err)
		return
	}

	resp := struct {
		JID string `json:"jid"`
	}{
		JID: curJob.JID,
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))

	al.Debugf("Job[id=%q] created to execute remote command on client with id=%q: %q.", curJob.JID, executeInput.ClientID, executeInput.Command)
}

func (al *APIListener) getClientForScriptExecution(clientID string) (*clients.Client, error) {
	client, err := al.clientService.GetActiveByID(clientID)
	if err != nil {
		return nil, errors2.APIError{
			Message:    fmt.Sprintf("Failed to find an active client with id=%q.", clientID),
			Err:        err,
			HTTPStatus: http.StatusInternalServerError,
		}
	}
	if client == nil {
		return nil, errors2.APIError{
			Message:    fmt.Sprintf("Active client with id=%q not found.", clientID),
			HTTPStatus: http.StatusNotFound,
		}
	}

	if client.Version != chshare.SourceVersion && client.Version < minVersionScriptExecSupport {
		return nil, errors2.APIError{
			Message:    fmt.Sprintf("Script Execution is supported starting from %s version, current client version is %s.", minVersionScriptExecSupport, client.Version),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	return client, nil
}

func (al *APIListener) handleExecuteScript(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routeParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
		return
	}

	execCmdInput := &api.ExecuteInput{}
	err := parseRequestBody(req.Body, &execCmdInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if execCmdInput.Script == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Missing script body")
		return
	}

	decodedScriptBytes, err := base64.StdEncoding.DecodeString(execCmdInput.Script)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}
	execCmdInput.Script = string(decodedScriptBytes)

	execCmdInput.ClientID = cid
	execCmdInput.IsScript = true

	cl, err := al.getClientForScriptExecution(cid)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	scriptPath, err := al.scriptManager.CreateScriptOnClient(execCmdInput, cl)
	if err != nil {
		if _, ok := err.(*comm.ClientError); ok {
			al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
		} else {
			al.jsonError(w, err)
		}
		return
	}
	execCmdInput.Command = scriptPath
	execCmdInput.HasShebang = script.HasShebangLine(execCmdInput.Script)

	al.handleExecuteCommand(req.Context(), w, execCmdInput)
}

func (al *APIListener) handleGetCommands(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routeParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
		return
	}

	res, err := al.jobProvider.GetSummariesByClientID(cid)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get client jobs: client_id=%q.", cid), err)
		return
	}

	jobs.SortByFinishedAt(res, true)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(res))
}

func (al *APIListener) handleGetCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routeParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamClientID))
		return
	}
	jid := vars[routeParamJobID]
	if jid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamJobID))
		return
	}

	job, err := al.jobProvider.GetByJID(cid, jid)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find a job[id=%q].", jid), err)
		return
	}
	if job == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Job[id=%q] not found.", jid))
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(job))
}

type newJobResponse struct {
	JID string `json:"jid"`
}

type multiClientCmdRequest struct {
	ClientIDs           []string `json:"client_ids"`
	ClientIDCommandMap  map[string]string
	OrderedClients      []*clients.Client
	GroupIDs            []string `json:"group_ids"`
	Command             string   `json:"command"`
	Script              string   `json:"script"`
	Cwd                 string   `json:"cwd"`
	IsSudo              bool     `json:"is_sudo"`
	Interpreter         string   `json:"interpreter"`
	TimeoutSec          int      `json:"timeout_sec"`
	ExecuteConcurrently bool     `json:"execute_concurrently"`
	AbortOnError        *bool    `json:"abort_on_error"` // pointer is used because it's default value is true. Otherwise it would be more difficult to check whether this field is missing or not
	IsScript            bool
	HasShebang          bool
}

// TODO: refactor to reuse similar code for REST API and WebSocket to execute cmds if both will be supported
func (al *APIListener) handlePostMultiClientCommand(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var reqBody multiClientCmdRequest
	err := parseRequestBody(req.Body, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if reqBody.Command == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Command cannot be empty.")
		return
	}
	if err := validation.ValidateInterpreter(reqBody.Interpreter, reqBody.IsScript); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid interpreter.", err)
		return
	}

	if reqBody.TimeoutSec <= 0 {
		reqBody.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	orderedClients, groupClientsCount, err := al.getOrderedClients(ctx, reqBody.ClientIDs, reqBody.GroupIDs)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if len(reqBody.GroupIDs) > 0 && groupClientsCount == 0 && len(reqBody.ClientIDs) == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "No active clients belong to the selected group(s).")
		return
	}

	minClients := 2
	if len(reqBody.ClientIDs) < minClients && groupClientsCount == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("At least %d clients should be specified.", minClients))
		return
	}

	// by default abortOnErr is true
	abortOnErr := true
	if reqBody.AbortOnError != nil {
		abortOnErr = *reqBody.AbortOnError
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.clientService.CheckClientsAccess(orderedClients, curUser)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	jid, err := generateNewJobID()
	if err != nil {
		al.jsonError(w, err)
		return
	}
	multiJob := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{
			JID:       jid,
			StartedAt: time.Now(),
			CreatedBy: curUser.Username,
		},
		ClientIDs:   reqBody.ClientIDs,
		GroupIDs:    reqBody.GroupIDs,
		Command:     reqBody.Command,
		Interpreter: reqBody.Interpreter,
		Cwd:         reqBody.Cwd,
		IsSudo:      reqBody.IsSudo,
		TimeoutSec:  reqBody.TimeoutSec,
		Concurrent:  reqBody.ExecuteConcurrently,
		AbortOnErr:  abortOnErr,
	}
	if err := al.jobProvider.SaveMultiJob(multiJob); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist a new multi-client job.", err)
		return
	}

	resp := newJobResponse{
		JID: multiJob.JID,
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))

	al.Debugf("Multi-client Job[id=%q] created to execute remote command on clients %s, groups %s: %q.", multiJob.JID, reqBody.ClientIDs, reqBody.GroupIDs, reqBody.Command)

	go al.executeMultiClientJob(multiJob, orderedClients, map[string]string{})
}

func (al *APIListener) getOrderedClients(
	ctx context.Context,
	clientIDs, groupIDs []string) (
	orderedClients []*clients.Client,
	groupClientsFoundCount int,
	err error,
) {
	var groups []*cgroups.ClientGroup
	for _, groupID := range groupIDs {
		group, err := al.clientGroupProvider.Get(ctx, groupID)
		if err != nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Failed to get a client group with id=%q.", groupID),
				Err:        err,
				HTTPStatus: http.StatusInternalServerError,
			}
			return orderedClients, groupClientsFoundCount, err
		}
		if group == nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Unknown group with id=%q.", groupID),
				Err:        err,
				HTTPStatus: http.StatusBadRequest,
			}
			return orderedClients, 0, err
		}
		groups = append(groups, group)
	}
	groupClients := al.clientService.GetActiveByGroups(groups)
	groupClientsFoundCount = len(groupClients)

	orderedClients = make([]*clients.Client, 0)
	usedClientIDs := make(map[string]bool)
	for _, cid := range clientIDs {
		client, err := al.clientService.GetByID(cid)
		if err != nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Failed to find a client with id=%q.", cid),
				Err:        err,
				HTTPStatus: http.StatusInternalServerError,
			}
			return orderedClients, 0, err
		}
		if client == nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Client with id=%q not found.", cid),
				Err:        err,
				HTTPStatus: http.StatusNotFound,
			}
			return orderedClients, 0, err
		}

		if client.DisconnectedAt != nil {
			err = errors2.APIError{
				Message:    fmt.Sprintf("Client with id=%q is not active.", cid),
				Err:        err,
				HTTPStatus: http.StatusBadRequest,
			}

			return orderedClients, 0, err
		}

		usedClientIDs[cid] = true
		orderedClients = append(orderedClients, client)
	}

	// append group clients
	for _, groupClient := range groupClients {
		if !usedClientIDs[groupClient.ID] {
			usedClientIDs[groupClient.ID] = true
			orderedClients = append(orderedClients, groupClient)
		}
	}

	return orderedClients, groupClientsFoundCount, nil
}

func (al *APIListener) executeMultiClientJob(
	job *models.MultiJob,
	orderedClients []*clients.Client,
	clientIDCommandMap map[string]string,
) {
	// for sequential execution - create a channel to get the job result
	var curJobDoneChannel chan *models.Job
	if !job.Concurrent {
		curJobDoneChannel = make(chan *models.Job)
		al.jobsDoneChannel.Set(job.JID, curJobDoneChannel)
		defer func() {
			close(curJobDoneChannel)
			al.jobsDoneChannel.Del(job.JID)
		}()
	}
	for _, client := range orderedClients {
		command, ok := clientIDCommandMap[client.ID]
		if !ok {
			command = job.Command
		}
		if job.Concurrent {
			go al.createAndRunJob(
				job.JID,
				command,
				job.Interpreter,
				job.CreatedBy,
				job.Cwd,
				job.TimeoutSec,
				job.IsSudo,
				job.IsScript,
				job.HasShebang,
				client,
			)
		} else {
			success := al.createAndRunJob(
				job.JID,
				command,
				job.Interpreter,
				job.CreatedBy,
				job.Cwd,
				job.TimeoutSec,
				job.IsSudo,
				job.IsScript,
				job.HasShebang,
				client,
			)
			if !success {
				if job.AbortOnErr {
					break
				}
				continue
			}

			// in tests skip next part to avoid waiting
			if al.insecureForTests {
				continue
			}

			// wait until command is finished
			jobResult := <-curJobDoneChannel
			if job.AbortOnErr && jobResult.Status == models.JobStatusFailed {
				break
			}
		}
	}
	if al.testDone != nil {
		al.testDone <- true
	}
}

func (al *APIListener) createAndRunJob(
	multiJobID, cmd, interpreter, createdBy, cwd string,
	timeoutSec int,
	isSudo, isScript, hasShebang bool,
	client *clients.Client,
) bool {
	jid, err := generateNewJobID()
	if err != nil {
		al.Errorf("multi_client_id=%q, client_id=%q, Could not generate job id: %v", multiJobID, client.ID, err)
		return false
	}
	// send the command to the client
	curJob := models.Job{
		JobSummary: models.JobSummary{
			JID: jid,
		},
		StartedAt:   time.Now(),
		ClientID:    client.ID,
		ClientName:  client.Name,
		Command:     cmd,
		Cwd:         cwd,
		IsSudo:      isSudo,
		IsScript:    isScript,
		Interpreter: interpreter,
		CreatedBy:   createdBy,
		TimeoutSec:  timeoutSec,
		MultiJobID:  &multiJobID,
		HasShebang:  hasShebang,
	}
	sshResp := &comm.RunCmdResponse{}
	err = comm.SendRequestAndGetResponse(client.Connection, comm.RequestTypeRunCmd, curJob, sshResp)
	// return an error after saving the job
	if err != nil {
		// failure, set fields to mark it as failed
		al.Errorf("multi_client_id=%q, client_id=%q, Error on execute remote command: %v", *curJob.MultiJobID, curJob.ClientID, err)
		curJob.Status = models.JobStatusFailed
		now := time.Now()
		curJob.FinishedAt = &now
		curJob.Error = err.Error()
	} else {
		// success, set fields received in response
		curJob.PID = &sshResp.Pid
		curJob.StartedAt = sshResp.StartedAt // override with the start time of the command
		curJob.Status = models.JobStatusRunning
	}

	if dbErr := al.jobProvider.CreateJob(&curJob); dbErr != nil {
		// just log it, cmd is running, when it's finished it can be saved on result return
		al.Errorf("multi_client_id=%q, client_id=%q, Failed to persist a child job: %v", *curJob.MultiJobID, curJob.ClientID, dbErr)
	}

	return err == nil
}

func (al *APIListener) handleCommandsWS(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	uiConn, err := apiUpgrader.Upgrade(w, req, nil)
	if err != nil {
		al.Errorf("Failed to establish WS connection: %v", err)
		return
	}
	uiConnTS := ws.NewConcurrentWebSocket(uiConn, al.Logger)
	inboundMsg := &multiClientCmdRequest{}
	err = uiConnTS.ReadJSON(inboundMsg)
	if err == io.EOF { // is handled separately to return an informative error message
		uiConnTS.WriteError("Inbound message should contain non empty json object with command data.", nil)
		return
	} else if err != nil {
		uiConnTS.WriteError("Invalid JSON data.", err)
		return
	}

	orderedClients, clientsInGroupsCount, err := al.getOrderedClients(ctx, inboundMsg.ClientIDs, inboundMsg.GroupIDs)
	if err != nil {
		uiConnTS.WriteError("", err)
		return
	}
	inboundMsg.OrderedClients = orderedClients

	al.handleCommandsExecutionWS(ctx, uiConnTS, inboundMsg, clientsInGroupsCount, map[string]string{})
}

func (al *APIListener) createScriptOnMultipleClients(
	ctx context.Context,
	inboundMsg *multiClientCmdRequest,
) (clientsInGroupsCount int, clientIDCommandMap map[string]string, err error) {
	if inboundMsg.Script == "" {
		return 0, nil, errors2.APIError{
			Message:    "Missing script body",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if inboundMsg.TimeoutSec <= 0 {
		inboundMsg.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	decodedScriptBytes, err := base64.StdEncoding.DecodeString(inboundMsg.Script)
	if err != nil {
		return 0, nil, errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
			Message:    "failed to decode script payload from base64",
		}
	}

	inboundMsg.Script = string(decodedScriptBytes)
	inboundMsg.IsScript = true

	orderedClients, clientsInGroupsCount, err := al.getOrderedClients(ctx, inboundMsg.ClientIDs, inboundMsg.GroupIDs)
	if err != nil {
		return 0, nil, err
	}
	if len(orderedClients) == 0 {
		return 0, nil, errors.New("no clients to execute the script for")
	}

	inboundMsg.OrderedClients = orderedClients

	clientIDCommandMap = make(map[string]string, len(orderedClients))
	for _, cl := range orderedClients {
		scriptPath, err := al.scriptManager.CreateScriptOnClient(
			&api.ExecuteInput{
				Command:     inboundMsg.Command,
				Script:      inboundMsg.Script,
				Interpreter: inboundMsg.Interpreter,
				Cwd:         inboundMsg.Cwd,
				IsSudo:      inboundMsg.IsSudo,
				TimeoutSec:  inboundMsg.TimeoutSec,
				ClientID:    cl.ID,
				IsScript:    true,
			},
			cl,
		)
		if err != nil {
			return 0, nil, err
		}
		clientIDCommandMap[cl.ID] = scriptPath
	}

	return clientsInGroupsCount, clientIDCommandMap, nil
}

func (al *APIListener) handleScriptsWS(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	uiConn, err := apiUpgrader.Upgrade(w, req, nil)
	if err != nil {
		al.Errorf("Failed to establish WS connection: %v", err)
		return
	}

	uiConnTS := ws.NewConcurrentWebSocket(uiConn, al.Logger)

	inboundMsg := &multiClientCmdRequest{}
	err = uiConnTS.ReadJSON(inboundMsg)
	if err == io.EOF { // is handled separately to return an informative error message
		uiConnTS.WriteError("Inbound message should contain non empty json object with command data.", nil)
		return
	}
	if err != nil {
		uiConnTS.WriteError("Invalid JSON data.", err)
		return
	}
	clientsInGroupsCount, clientIDCommandMap, err := al.createScriptOnMultipleClients(ctx, inboundMsg)
	if err != nil {
		uiConnTS.WriteError("Failed to create script on multiple clients", err)
		return
	}
	inboundMsg.HasShebang = script.HasShebangLine(inboundMsg.Script)

	al.handleCommandsExecutionWS(ctx, uiConnTS, inboundMsg, clientsInGroupsCount, clientIDCommandMap)
}

func (al *APIListener) handleCommandsExecutionWS(
	ctx context.Context,
	uiConnTS *ws.ConcurrentWebSocket,
	inboundMsg *multiClientCmdRequest,
	clientsInGroupsCount int,
	clientIDCommandMap map[string]string,
) {
	if inboundMsg.Command == "" && len(clientIDCommandMap) == 0 {
		uiConnTS.WriteError("Command cannot be empty.", nil)
		return
	}
	if err := validation.ValidateInterpreter(inboundMsg.Interpreter, inboundMsg.IsScript); err != nil {
		uiConnTS.WriteError("Invalid interpreter", err)
		return
	}

	if inboundMsg.TimeoutSec <= 0 {
		inboundMsg.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	if len(inboundMsg.GroupIDs) > 0 && clientsInGroupsCount == 0 && len(inboundMsg.ClientIDs) == 0 {
		uiConnTS.WriteError("No active clients belong to the selected group(s).", nil)
		return
	}

	if len(inboundMsg.ClientIDs) < 1 && clientsInGroupsCount == 0 {
		uiConnTS.WriteError("'client_ids' field should contain at least one client ID", nil)
		return
	}

	curUser, err := al.getUserModelForAuth(ctx)
	if err != nil {
		uiConnTS.WriteError("Could not get current user.", err)
		return
	}

	err = al.clientService.CheckClientsAccess(inboundMsg.OrderedClients, curUser)
	if err != nil {
		uiConnTS.WriteError(err.Error(), nil)
		return
	}

	jid, err := generateNewJobID()
	if err != nil {
		uiConnTS.WriteError("Could not generate job id.", err)
		return
	}
	al.Server.uiJobWebSockets.Set(jid, uiConnTS)
	defer al.Server.uiJobWebSockets.Delete(jid)

	createdBy := curUser.Username
	if len(inboundMsg.ClientIDs) > 1 || clientsInGroupsCount > 0 {
		// by default abortOnErr is true
		abortOnErr := true
		if inboundMsg.AbortOnError != nil {
			abortOnErr = *inboundMsg.AbortOnError
		}

		multiJob := &models.MultiJob{
			MultiJobSummary: models.MultiJobSummary{
				JID:       jid,
				StartedAt: time.Now(),
				CreatedBy: createdBy,
			},
			ClientIDs:   inboundMsg.ClientIDs,
			GroupIDs:    inboundMsg.GroupIDs,
			Command:     inboundMsg.Command,
			Cwd:         inboundMsg.Cwd,
			Interpreter: inboundMsg.Interpreter,
			TimeoutSec:  inboundMsg.TimeoutSec,
			Concurrent:  inboundMsg.ExecuteConcurrently,
			AbortOnErr:  abortOnErr,
			IsSudo:      inboundMsg.IsSudo,
			IsScript:    inboundMsg.IsScript,
			HasShebang:  inboundMsg.HasShebang,
		}
		if err := al.jobProvider.SaveMultiJob(multiJob); err != nil {
			uiConnTS.WriteError("Failed to persist a new multi-client job.", err)
			return
		}

		al.Debugf("Multi-client Job[id=%q] created to execute remote command on clients %s, groups %s: %q.", multiJob.JID, inboundMsg.ClientIDs, inboundMsg.GroupIDs, inboundMsg.Command)
		uiConnTS.SetWritesBeforeClose(len(inboundMsg.OrderedClients))

		// for sequential execution - create a channel to get the job result
		var curJobDoneChannel chan *models.Job
		if !multiJob.Concurrent {
			curJobDoneChannel = make(chan *models.Job)
			al.jobsDoneChannel.Set(multiJob.JID, curJobDoneChannel)
			defer func() {
				close(curJobDoneChannel)
				al.jobsDoneChannel.Del(multiJob.JID)
			}()
		}

		for _, client := range inboundMsg.OrderedClients {
			command, ok := clientIDCommandMap[client.ID]
			if !ok {
				command = inboundMsg.Command
			}

			curJID, err := generateNewJobID()
			if err != nil {
				uiConnTS.WriteError("Could not generate job id.", err)
				return
			}
			if multiJob.Concurrent {
				go al.createAndRunJobWS(
					uiConnTS,
					&jid,
					curJID,
					command,
					multiJob.Interpreter,
					createdBy,
					multiJob.Cwd,
					multiJob.TimeoutSec,
					multiJob.IsSudo,
					multiJob.IsScript,
					multiJob.HasShebang,
					client,
				)
			} else {
				success := al.createAndRunJobWS(
					uiConnTS,
					&jid,
					curJID,
					command,
					multiJob.Interpreter,
					createdBy,
					multiJob.Cwd,
					multiJob.TimeoutSec,
					multiJob.IsSudo,
					multiJob.IsScript,
					multiJob.HasShebang,
					client,
				)
				if !success {
					if multiJob.AbortOnErr {
						uiConnTS.Close()
						return
					}
					continue
				}
				// wait until command is finished
				jobResult := <-curJobDoneChannel
				if multiJob.AbortOnErr && jobResult.Status == models.JobStatusFailed {
					uiConnTS.Close()
					return
				}
			}
		}
	} else {
		client := inboundMsg.OrderedClients[0]
		command, ok := clientIDCommandMap[client.ID]
		if !ok {
			command = inboundMsg.Command
		}

		al.createAndRunJobWS(
			uiConnTS,
			nil,
			jid,
			command,
			inboundMsg.Interpreter,
			createdBy,
			inboundMsg.Cwd,
			inboundMsg.TimeoutSec,
			inboundMsg.IsSudo,
			inboundMsg.IsScript,
			inboundMsg.HasShebang,
			client,
		)
	}

	// check for Close message from client to close the connection
	mt, message, err := uiConnTS.ReadMessage()
	if err != nil {
		if closeErr, ok := err.(*websocket.CloseError); ok {
			al.Debugf("Received a closed err on WS read: %v", closeErr)
			return
		}
		al.Debugf("Error read from websocket: %v", err)
		return
	}

	al.Debugf("Message received: type %v, msg %s", mt, message)
	uiConnTS.Close()
}

func (al *APIListener) createAndRunJobWS(
	uiConnTS *ws.ConcurrentWebSocket,
	multiJobID *string,
	jid, cmd, interpreter, createdBy, cwd string,
	timeoutSec int,
	isSudo, isScript, hasShebang bool,
	client *clients.Client,
) bool {
	curJob := models.Job{
		JobSummary: models.JobSummary{
			JID: jid,
		},
		StartedAt:   time.Now(),
		ClientID:    client.ID,
		ClientName:  client.Name,
		Command:     cmd,
		Interpreter: interpreter,
		CreatedBy:   createdBy,
		TimeoutSec:  timeoutSec,
		MultiJobID:  multiJobID,
		Cwd:         cwd,
		IsSudo:      isSudo,
		IsScript:    isScript,
		HasShebang:  hasShebang,
	}
	logPrefix := curJob.LogPrefix()

	// send the command to the client
	sshResp := &comm.RunCmdResponse{}
	err := comm.SendRequestAndGetResponse(client.Connection, comm.RequestTypeRunCmd, curJob, sshResp)
	if err != nil {
		al.Errorf("%s, Error on execute remote command: %v", logPrefix, err)

		curJob.Status = models.JobStatusFailed
		now := time.Now()
		curJob.FinishedAt = &now
		curJob.Error = err.Error()

		// send the failed job to UI
		_ = uiConnTS.WriteJSON(curJob)
	} else {
		al.Debugf("%s, Job was sent to execute remote command: %q.", logPrefix, curJob.Command)

		// success, set fields received in response
		curJob.PID = &sshResp.Pid
		curJob.StartedAt = sshResp.StartedAt // override with the start time of the command
		curJob.Status = models.JobStatusRunning
	}

	// do not save the failed job if it's a single-client job
	if err != nil && multiJobID == nil {
		return false
	}

	if dbErr := al.jobProvider.CreateJob(&curJob); dbErr != nil {
		// just log it, cmd is running, when it's finished it can be saved on result return
		al.Errorf("%s, Failed to persist job: %v", logPrefix, dbErr)
	}

	return err == nil
}

func (al *APIListener) handleGetMultiClientCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	jid := vars[routeParamJobID]
	if jid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamJobID))
		return
	}

	job, err := al.jobProvider.GetMultiJob(jid)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find a multi-client job[id=%q].", jid), err)
		return
	}
	if job == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Multi-client Job[id=%q] not found.", jid))
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(job))
}

func (al *APIListener) handleGetMultiClientCommands(w http.ResponseWriter, req *http.Request) {
	res, err := al.jobProvider.GetAllMultiJobSummaries()
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to get multi-client jobs.", err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(res))
}

func (al *APIListener) handlePostClientGroups(w http.ResponseWriter, req *http.Request) {
	var group cgroups.ClientGroup
	err := parseRequestBody(req.Body, &group)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if err := validateInputClientGroup(group); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid client group.", err)
		return
	}

	if err := al.clientGroupProvider.Create(req.Context(), &group); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist a new client group.", err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	al.Debugf("Client Group [id=%q] created.", group.ID)
}

func (al *APIListener) handlePutClientGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	id := vars[routeParamGroupID]
	if id == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamGroupID))
		return
	}

	var group cgroups.ClientGroup
	err := parseRequestBody(req.Body, &group)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if id != group.ID {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("%q route param doesn't not match group ID from request body.", routeParamGroupID))
		return
	}

	if err := validateInputClientGroup(group); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid client group.", err)
		return
	}

	if err := al.clientGroupProvider.Update(req.Context(), &group); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist client group.", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	al.Debugf("Client Group [id=%q] updated.", group.ID)
}

const groupIDMaxLength = 30
const validGroupIDChars = "A-Za-z0-9_-*"

var invalidGroupIDRegexp = regexp.MustCompile(`[^\*A-Za-z0-9_-]`)

func validateInputClientGroup(group cgroups.ClientGroup) error {
	if strings.TrimSpace(group.ID) == "" {
		return errors.New("group ID cannot be empty")
	}
	if len(group.ID) > groupIDMaxLength {
		return fmt.Errorf("invalid group ID: max length %d, got %d", groupIDMaxLength, len(group.ID))
	}
	if invalidGroupIDRegexp.MatchString(group.ID) {
		return fmt.Errorf("invalid group ID %q: can contain only %q", group.ID, validGroupIDChars)
	}
	return nil
}

func (al *APIListener) handleGetClientGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	id := vars[routeParamGroupID]
	if id == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamGroupID))
		return
	}

	group, err := al.clientGroupProvider.Get(req.Context(), id)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find client group[id=%q].", id), err)
		return
	}
	if group == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Client Group[id=%q] not found.", id))
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.clientService.PopulateGroupsWithUserClients([]*cgroups.ClientGroup{group}, curUser)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(group))
}

func (al *APIListener) handleGetClientGroups(w http.ResponseWriter, req *http.Request) {
	res, err := al.clientGroupProvider.GetAll(req.Context())
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to get client groups.", err)
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.clientService.PopulateGroupsWithUserClients(res, curUser)

	// for non-admins filter out groups with no clients
	if !curUser.IsAdmin() {
		res = filterEmptyGroups(res)
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(res))
}

func filterEmptyGroups(groups []*cgroups.ClientGroup) []*cgroups.ClientGroup {
	var nonEmptyGroups []*cgroups.ClientGroup
	for _, group := range groups {
		if len(group.ClientIDs) > 0 {
			nonEmptyGroups = append(nonEmptyGroups, group)
		}
	}
	return nonEmptyGroups
}

func (al *APIListener) handleDeleteClientGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	id := vars[routeParamGroupID]
	if id == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routeParamGroupID))
		return
	}

	err := al.clientGroupProvider.Delete(req.Context(), id)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete client group[id=%q].", id), err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	al.Debugf("Client Group [id=%q] deleted.", id)
}

func (al *APIListener) wrapStaticPassModeMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if al.usersService.GetProviderType() == enums.ProviderSourceStatic {
			al.jsonError(w, errors2.APIError{
				HTTPStatus: http.StatusBadRequest,
				Message:    "server runs on a static user-password pair, please use JSON file or database for user data",
			})
			return
		}
		next.ServeHTTP(w, r)
	}
}

func (al *APIListener) wrapAdminAccessMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if al.insecureForTests {
			next.ServeHTTP(w, r)
			return
		}

		user, err := al.getUserModelForAuth(r.Context())
		if err != nil {
			al.jsonError(w, err)
			return
		}

		if user.IsAdmin() {
			next.ServeHTTP(w, r)
			return
		}

		al.jsonError(w, errors2.APIError{
			Message: fmt.Sprintf(
				"current user should belong to %s group to access this resource",
				users.Administrators,
			),
			HTTPStatus: http.StatusForbidden,
		})
	}
}

func (al *APIListener) handleGetVaultStatus(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	st, err := al.vaultManager.Status(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(st))
}

func (al *APIListener) handleVaultUnlock(w http.ResponseWriter, req *http.Request) {
	var passReq vault.PassRequest
	err := parseRequestBody(req.Body, &passReq)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.vaultManager.UnLock(req.Context(), passReq.Password)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (al *APIListener) handleVaultLock(w http.ResponseWriter, req *http.Request) {
	err := al.vaultManager.Lock(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handleVaultInit(w http.ResponseWriter, req *http.Request) {
	var passReq vault.PassRequest
	err := parseRequestBody(req.Body, &passReq)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.vaultManager.Init(req.Context(), passReq.Password)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (al *APIListener) handleListVaultValues(w http.ResponseWriter, req *http.Request) {
	items, err := al.vaultManager.List(req.Context(), req)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(items))
}

func (al *APIListener) readIntParam(paramName string, req *http.Request) (int, error) {
	vars := mux.Vars(req)
	idStr, ok := vars[paramName]
	if !ok {
		return 0, nil
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, fmt.Errorf("Non-numeric integer value provided: %s for param %s", idStr, paramName)
	}

	return id, nil
}

func (al *APIListener) handleReadVaultValue(w http.ResponseWriter, req *http.Request) {
	id, err := al.readIntParam(routeParamVaultValueID, req)
	if err != nil {
		al.jsonError(w, errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}
	if id == 0 {
		al.jsonError(w, errors2.APIError{
			Err:        fmt.Errorf("missing %q route param", routeParamVaultValueID),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	storedValue, found, err := al.vaultManager.GetOne(req.Context(), id, curUser)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Cannot find a vault value by the provided id: %d", id))
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleVaultStoreValue(w http.ResponseWriter, req *http.Request) {
	id, err := al.readIntParam(routeParamVaultValueID, req)
	if err != nil {
		al.jsonError(w, errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	var vaultKeyValue vault.InputValue
	err = parseRequestBody(req.Body, &vaultKeyValue)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	storedValue, err := al.vaultManager.Store(req.Context(), int64(id), &vaultKeyValue, curUser)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	status := http.StatusOK

	if id == 0 {
		w.WriteHeader(http.StatusCreated)
	}

	al.writeJSONResponse(w, status, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleVaultDeleteValue(w http.ResponseWriter, req *http.Request) {
	id, err := al.readIntParam(routeParamVaultValueID, req)
	if err != nil {
		al.jsonError(w, errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}
	if id == 0 {
		al.jsonError(w, errors2.APIError{
			Err:        fmt.Errorf("missing %q route param", routeParamVaultValueID),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.vaultManager.Delete(req.Context(), id, curUser)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handleListScripts(w http.ResponseWriter, req *http.Request) {
	items, err := al.scriptManager.List(req.Context(), req)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(items))
}

func (al *APIListener) handleScriptCreate(w http.ResponseWriter, req *http.Request) {
	var scriptInput script.InputScript
	err := parseRequestBody(req.Body, &scriptInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	storedValue, err := al.scriptManager.Create(req.Context(), &scriptInput, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)

	al.writeJSONResponse(w, http.StatusCreated, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleScriptUpdate(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr, ok := vars[routeParamScriptValueID]
	if !ok {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Script ID is not provided")
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var scriptInput script.InputScript
	err := parseRequestBody(req.Body, &scriptInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	storedValue, err := al.scriptManager.Update(req.Context(), idStr, &scriptInput, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleReadScript(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routeParamScriptValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty script id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	foundScript, found, err := al.scriptManager.GetOne(req.Context(), idStr)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Cannot find a script by the provided id: %s", idStr))
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(foundScript))
}

func (al *APIListener) handleDeleteScript(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routeParamScriptValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty script id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	err := al.scriptManager.Delete(req.Context(), idStr)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func parseRequestBody(reqBody io.ReadCloser, dest interface{}) error {
	dec := json.NewDecoder(reqBody)
	dec.DisallowUnknownFields()
	err := dec.Decode(dest)
	if err == io.EOF { // is handled separately to return an informative error message
		return errors2.APIError{
			Message:    "Missing body with json data.",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if err != nil {
		return errors2.APIError{
			Message:    "Invalid JSON data.",
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	return nil
}

func (al *APIListener) handleRefreshUpdatesStatus(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	clientID := vars[routeParamClientID]
	if clientID == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "client id is missing")
		return
	}

	client, err := al.clientService.GetActiveByID(clientID)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("client with id %s not found", clientID))
		return
	}

	err = comm.SendRequestAndGetResponse(client.Connection, comm.RequestTypeRefreshUpdatesStatus, nil, nil)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handlePostMultiClientScript(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	inboundMsg := new(multiClientCmdRequest)
	err := parseRequestBody(req.Body, inboundMsg)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	groupClientsCount, clientIDCommandMap, err := al.createScriptOnMultipleClients(ctx, inboundMsg)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if len(inboundMsg.GroupIDs) > 0 && groupClientsCount == 0 && len(inboundMsg.ClientIDs) == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "No active clients belong to the selected group(s).")
		return
	}

	minClients := 2
	if len(inboundMsg.ClientIDs) < minClients && groupClientsCount == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("At least %d clients should be specified.", minClients))
		return
	}

	// by default abortOnErr is true
	abortOnErr := true
	if inboundMsg.AbortOnError != nil {
		abortOnErr = *inboundMsg.AbortOnError
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	err = al.clientService.CheckClientsAccess(inboundMsg.OrderedClients, curUser)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	jid, err := generateNewJobID()
	if err != nil {
		al.jsonError(w, err)
		return
	}

	multiJob := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{
			JID:       jid,
			StartedAt: time.Now(),
			CreatedBy: curUser.Username,
		},
		ClientIDs:   inboundMsg.ClientIDs,
		GroupIDs:    inboundMsg.GroupIDs,
		Command:     inboundMsg.Command,
		Interpreter: inboundMsg.Interpreter,
		Cwd:         inboundMsg.Cwd,
		IsSudo:      inboundMsg.IsSudo,
		TimeoutSec:  inboundMsg.TimeoutSec,
		Concurrent:  inboundMsg.ExecuteConcurrently,
		AbortOnErr:  abortOnErr,
		HasShebang:  script.HasShebangLine(inboundMsg.Script),
	}
	if err := al.jobProvider.SaveMultiJob(multiJob); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist a new multi-client job.", err)
		return
	}

	resp := newJobResponse{
		JID: multiJob.JID,
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))

	al.Debugf("Multi-client Job[id=%q] created to execute remote command on clients %s, groups %s: %q.", multiJob.JID, inboundMsg.ClientIDs, inboundMsg.GroupIDs, inboundMsg.Command)

	go al.executeMultiClientJob(multiJob, inboundMsg.OrderedClients, clientIDCommandMap)
}

type postTokenResponse struct {
	Token string `json:"token"`
}

func (al *APIListener) handlePostToken(w http.ResponseWriter, req *http.Request) {
	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	newToken, err := random.UUID4()
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if err := al.usersService.Change(&users.User{
		Token: &newToken,
	}, curUser.Username); err != nil {
		al.jsonError(w, err)
		return
	}

	resp := postTokenResponse{
		Token: newToken,
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))
}

func (al *APIListener) handleDeleteToken(w http.ResponseWriter, req *http.Request) {
	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	noToken := ""
	if err := al.usersService.Change(&users.User{
		Token: &noToken,
	}, curUser.Username); err != nil {
		al.jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
