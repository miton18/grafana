package api

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/metrics"
	"github.com/grafana/grafana/pkg/middleware"
	m "github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/social"
)

func GenStateString() string {
	rnd := make([]byte, 32)
	rand.Read(rnd)
	return base64.StdEncoding.EncodeToString(rnd)
}

func OAuthLogin(ctx *middleware.Context) {
	if setting.OAuthService == nil {
		ctx.Handle(404, "login.OAuthLogin(oauth service not enabled)", nil)
		return
	}

	name := ctx.Params(":name")
	connect, ok := social.SocialMap[name]
	if !ok {
		ctx.Handle(404, "login.OAuthLogin(social login not enabled)", errors.New(name))
		return
	}

	error := ctx.Query("error")
	if error != "" {
		errorDesc := ctx.Query("error_description")
		ctx.Logger.Info("OAuthLogin Failed", "error", error, "errorDesc", errorDesc)
		ctx.Redirect(setting.AppSubUrl + "/login?failCode=1003")
		return
	}

	code := ctx.Query("code")
	if code == "" {
		state := GenStateString()
		ctx.Session.Set(middleware.SESS_KEY_OAUTH_STATE, state)
		ctx.Redirect(connect.AuthCodeURL(state, oauth2.AccessTypeOnline))
		return
	}

	// verify state string
	savedState := ctx.Session.Get(middleware.SESS_KEY_OAUTH_STATE).(string)
	queryState := ctx.Query("state")
	if savedState != queryState {
		ctx.Handle(500, "login.OAuthLogin(state mismatch)", nil)
		return
	}

	// handle call back

	// initialize oauth2 context
	oauthCtx := oauth2.NoContext
	if setting.OAuthService.OAuthInfos[name].TlsClientCert != "" {
		cert, err := tls.LoadX509KeyPair(setting.OAuthService.OAuthInfos[name].TlsClientCert, setting.OAuthService.OAuthInfos[name].TlsClientKey)
		if err != nil {
			log.Fatal(err)
		}

		// Load CA cert
		caCert, err := ioutil.ReadFile(setting.OAuthService.OAuthInfos[name].TlsClientCa)
		if err != nil {
			log.Fatal(err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				Certificates: []tls.Certificate{cert},
				RootCAs: caCertPool,
			},
		}
		sslcli := &http.Client{Transport: tr}

		oauthCtx = context.TODO()
		oauthCtx = context.WithValue(oauthCtx, oauth2.HTTPClient, sslcli)
	}

	// get token from provider
	token, err := connect.Exchange(oauthCtx, code)
	if err != nil {
		ctx.Handle(500, "login.OAuthLogin(NewTransportWithCode)", err)
		return
	}

	ctx.Logger.Debug("OAuthLogin Got token")

	// set up oauth2 client
	client := connect.Client(oauthCtx, token)

	// get user info
	userInfo, err := connect.UserInfo(client)
	if err != nil {
		if err == social.ErrMissingTeamMembership {
			ctx.Redirect(setting.AppSubUrl + "/login?failCode=1000")
		} else if err == social.ErrMissingOrganizationMembership {
			ctx.Redirect(setting.AppSubUrl + "/login?failCode=1001")
		} else {
			ctx.Handle(500, fmt.Sprintf("login.OAuthLogin(get info from %s)", name), err)
		}
		return
	}

	ctx.Logger.Debug("OAuthLogin got user info", "userInfo", userInfo)

	// validate that the email is allowed to login to grafana
	if !connect.IsEmailAllowed(userInfo.Email) {
		ctx.Logger.Info("OAuth login attempt with unallowed email", "email", userInfo.Email)
		ctx.Redirect(setting.AppSubUrl + "/login?failCode=1002")
		return
	}

	userQuery := m.GetUserByEmailQuery{Email: userInfo.Email}
	err = bus.Dispatch(&userQuery)

	// create account if missing
	if err == m.ErrUserNotFound {
		if !connect.IsSignupAllowed() {
			ctx.Redirect(setting.AppSubUrl + "/login")
			return
		}
		limitReached, err := middleware.QuotaReached(ctx, "user")
		if err != nil {
			ctx.Handle(500, "Failed to get user quota", err)
			return
		}
		if limitReached {
			ctx.Redirect(setting.AppSubUrl + "/login")
			return
		}
		cmd := m.CreateUserCommand{
			Login:          userInfo.Login,
			Email:          userInfo.Email,
			Name:           userInfo.Name,
			Company:        userInfo.Company,
			DefaultOrgRole: userInfo.Role,
		}

		if err = bus.Dispatch(&cmd); err != nil {
			ctx.Handle(500, "Failed to create account", err)
			return
		}

		userQuery.Result = &cmd.Result
	} else if err != nil {
		ctx.Handle(500, "Unexpected error", err)
	}

	// login
	loginUserWithUser(userQuery.Result, ctx)

	metrics.M_Api_Login_OAuth.Inc(1)

	ctx.Redirect(setting.AppSubUrl + "/")
}
