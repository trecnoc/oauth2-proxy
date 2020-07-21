package main

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/justinas/alice"
	ipapi "github.com/oauth2-proxy/oauth2-proxy/pkg/apis/ip"
	middlewareapi "github.com/oauth2-proxy/oauth2-proxy/pkg/apis/middleware"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/apis/options"
	sessionsapi "github.com/oauth2-proxy/oauth2-proxy/pkg/apis/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/authentication/basic"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/cookies"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/encryption"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/ip"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/logger"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/middleware"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/upstream"
	"github.com/oauth2-proxy/oauth2-proxy/providers"
)

const (
	httpScheme  = "http"
	httpsScheme = "https"

	applicationJSON = "application/json"
)

var (
	// ErrNeedsLogin means the user should be redirected to the login page
	ErrNeedsLogin = errors.New("redirect to login page")

	// Used to check final redirects are not susceptible to open redirects.
	// Matches //, /\ and both of these with whitespace in between (eg / / or / \).
	invalidRedirectRegex = regexp.MustCompile(`[/\\](?:[\s\v]*|\.{1,2})[/\\]`)
)

// OAuthProxy is the main authentication proxy
type OAuthProxy struct {
	CookieSeed     string
	CookieName     string
	CSRFCookieName string
	CookieDomains  []string
	CookiePath     string
	CookieSecure   bool
	CookieHTTPOnly bool
	CookieExpire   time.Duration
	CookieRefresh  time.Duration
	CookieSameSite string
	Validator      func(string) bool

	RobotsPath        string
	SignInPath        string
	SignOutPath       string
	OAuthStartPath    string
	OAuthCallbackPath string
	AuthOnlyPath      string
	UserInfoPath      string

	redirectURL             *url.URL // the url to receive requests at
	whitelistDomains        []string
	provider                providers.Provider
	providerNameOverride    string
	sessionStore            sessionsapi.SessionStore
	ProxyPrefix             string
	SignInMessage           string
	basicAuthValidator      basic.Validator
	displayHtpasswdForm     bool
	serveMux                http.Handler
	SetXAuthRequest         bool
	PassBasicAuth           bool
	SetBasicAuth            bool
	SkipProviderButton      bool
	PassUserHeaders         bool
	BasicAuthPassword       string
	PassAccessToken         bool
	SetAuthorization        bool
	PassAuthorization       bool
	PreferEmailToUser       bool
	skipAuthRegex           []string
	skipAuthPreflight       bool
	skipAuthStripHeaders    bool
	skipJwtBearerTokens     bool
	mainJwtBearerVerifier   *oidc.IDTokenVerifier
	extraJwtBearerVerifiers []*oidc.IDTokenVerifier
	compiledRegex           []*regexp.Regexp
	templates               *template.Template
	realClientIPParser      ipapi.RealClientIPParser
	trustedIPs              *ip.NetSet
	Banner                  string
	Footer                  string

	sessionChain alice.Chain
}

// NewOAuthProxy creates a new instance of OAuthProxy from the options provided
func NewOAuthProxy(opts *options.Options, validator func(string) bool) (*OAuthProxy, error) {
	sessionStore, err := sessions.NewSessionStore(&opts.Session, &opts.Cookie)
	if err != nil {
		return nil, fmt.Errorf("error initialising session store: %v", err)
	}

	templates := loadTemplates(opts.CustomTemplatesDir)
	proxyErrorHandler := upstream.NewProxyErrorHandler(templates.Lookup("error.html"), opts.ProxyPrefix)
	upstreamProxy, err := upstream.NewProxy(opts.UpstreamServers, opts.GetSignatureData(), proxyErrorHandler)
	if err != nil {
		return nil, fmt.Errorf("error initialising upstream proxy: %v", err)
	}

	for _, u := range opts.GetCompiledRegex() {
		logger.Printf("compiled skip-auth-regex => %q", u)
	}

	if opts.SkipJwtBearerTokens {
		logger.Printf("Skipping JWT tokens from configured OIDC issuer: %q", opts.OIDCIssuerURL)
		for _, issuer := range opts.ExtraJwtIssuers {
			logger.Printf("Skipping JWT tokens from extra JWT issuer: %q", issuer)
		}
	}
	redirectURL := opts.GetRedirectURL()
	if redirectURL.Path == "" {
		redirectURL.Path = fmt.Sprintf("%s/callback", opts.ProxyPrefix)
	}

	logger.Printf("OAuthProxy configured for %s Client ID: %s", opts.GetProvider().Data().ProviderName, opts.ClientID)
	refresh := "disabled"
	if opts.Cookie.Refresh != time.Duration(0) {
		refresh = fmt.Sprintf("after %s", opts.Cookie.Refresh)
	}

	logger.Printf("Cookie settings: name:%s secure(https):%v httponly:%v expiry:%s domains:%s path:%s samesite:%s refresh:%s", opts.Cookie.Name, opts.Cookie.Secure, opts.Cookie.HTTPOnly, opts.Cookie.Expire, strings.Join(opts.Cookie.Domains, ","), opts.Cookie.Path, opts.Cookie.SameSite, refresh)

	trustedIPs := ip.NewNetSet()
	for _, ipStr := range opts.TrustedIPs {
		if ipNet := ip.ParseIPNet(ipStr); ipNet != nil {
			trustedIPs.AddIPNet(*ipNet)
		} else {
			return nil, fmt.Errorf("could not parse IP network (%s)", ipStr)
		}
	}

	var basicAuthValidator basic.Validator
	if opts.HtpasswdFile != "" {
		logger.Printf("using htpasswd file: %s", opts.HtpasswdFile)
		var err error
		basicAuthValidator, err = basic.NewHTPasswdValidator(opts.HtpasswdFile)
		if err != nil {
			return nil, fmt.Errorf("could not load htpasswdfile: %v", err)
		}
	}

	sessionChain := buildSessionChain(opts, sessionStore, basicAuthValidator)

	return &OAuthProxy{
		CookieName:     opts.Cookie.Name,
		CSRFCookieName: fmt.Sprintf("%v_%v", opts.Cookie.Name, "csrf"),
		CookieSeed:     opts.Cookie.Secret,
		CookieDomains:  opts.Cookie.Domains,
		CookiePath:     opts.Cookie.Path,
		CookieSecure:   opts.Cookie.Secure,
		CookieHTTPOnly: opts.Cookie.HTTPOnly,
		CookieExpire:   opts.Cookie.Expire,
		CookieRefresh:  opts.Cookie.Refresh,
		CookieSameSite: opts.Cookie.SameSite,
		Validator:      validator,

		RobotsPath:        "/robots.txt",
		SignInPath:        fmt.Sprintf("%s/sign_in", opts.ProxyPrefix),
		SignOutPath:       fmt.Sprintf("%s/sign_out", opts.ProxyPrefix),
		OAuthStartPath:    fmt.Sprintf("%s/start", opts.ProxyPrefix),
		OAuthCallbackPath: fmt.Sprintf("%s/callback", opts.ProxyPrefix),
		AuthOnlyPath:      fmt.Sprintf("%s/auth", opts.ProxyPrefix),
		UserInfoPath:      fmt.Sprintf("%s/userinfo", opts.ProxyPrefix),

		ProxyPrefix:             opts.ProxyPrefix,
		provider:                opts.GetProvider(),
		providerNameOverride:    opts.ProviderName,
		sessionStore:            sessionStore,
		serveMux:                upstreamProxy,
		redirectURL:             redirectURL,
		whitelistDomains:        opts.WhitelistDomains,
		skipAuthRegex:           opts.SkipAuthRegex,
		skipAuthPreflight:       opts.SkipAuthPreflight,
		skipAuthStripHeaders:    opts.SkipAuthStripHeaders,
		skipJwtBearerTokens:     opts.SkipJwtBearerTokens,
		mainJwtBearerVerifier:   opts.GetOIDCVerifier(),
		extraJwtBearerVerifiers: opts.GetJWTBearerVerifiers(),
		compiledRegex:           opts.GetCompiledRegex(),
		realClientIPParser:      opts.GetRealClientIPParser(),
		SetXAuthRequest:         opts.SetXAuthRequest,
		PassBasicAuth:           opts.PassBasicAuth,
		SetBasicAuth:            opts.SetBasicAuth,
		PassUserHeaders:         opts.PassUserHeaders,
		BasicAuthPassword:       opts.BasicAuthPassword,
		PassAccessToken:         opts.PassAccessToken,
		SetAuthorization:        opts.SetAuthorization,
		PassAuthorization:       opts.PassAuthorization,
		PreferEmailToUser:       opts.PreferEmailToUser,
		SkipProviderButton:      opts.SkipProviderButton,
		templates:               templates,
		trustedIPs:              trustedIPs,
		Banner:                  opts.Banner,
		Footer:                  opts.Footer,

		basicAuthValidator:  basicAuthValidator,
		displayHtpasswdForm: basicAuthValidator != nil,
		sessionChain:        sessionChain,
	}, nil
}

func buildSessionChain(opts *options.Options, sessionStore sessionsapi.SessionStore, validator basic.Validator) alice.Chain {
	chain := alice.New(middleware.NewScope())

	if opts.SkipJwtBearerTokens {
		sessionLoaders := []middlewareapi.TokenToSessionLoader{}
		if opts.GetOIDCVerifier() != nil {
			sessionLoaders = append(sessionLoaders, middlewareapi.TokenToSessionLoader{
				Verifier:       opts.GetOIDCVerifier(),
				TokenToSession: opts.GetProvider().CreateSessionStateFromBearerToken,
			})
		}

		for _, verifier := range opts.GetJWTBearerVerifiers() {
			sessionLoaders = append(sessionLoaders, middlewareapi.TokenToSessionLoader{
				Verifier: verifier,
			})
		}

		chain = chain.Append(middleware.NewJwtSessionLoader(sessionLoaders))
	}

	if validator != nil {
		chain = chain.Append(middleware.NewBasicAuthSessionLoader(validator))
	}

	chain = chain.Append(middleware.NewStoredSessionLoader(&middleware.StoredSessionLoaderOptions{
		SessionStore:           sessionStore,
		RefreshPeriod:          opts.Cookie.Refresh,
		RefreshSessionIfNeeded: opts.GetProvider().RefreshSessionIfNeeded,
		ValidateSessionState:   opts.GetProvider().ValidateSessionState,
	}))

	return chain
}

// GetRedirectURI returns the redirectURL that the upstream OAuth Provider will
// redirect clients to once authenticated
func (p *OAuthProxy) GetRedirectURI(host string) string {
	// default to the request Host if not set
	if p.redirectURL.Host != "" {
		return p.redirectURL.String()
	}
	u := *p.redirectURL
	if u.Scheme == "" {
		if p.CookieSecure {
			u.Scheme = httpsScheme
		} else {
			u.Scheme = httpScheme
		}
	}
	u.Host = host
	return u.String()
}

func (p *OAuthProxy) redeemCode(ctx context.Context, host, code string) (s *sessionsapi.SessionState, err error) {
	if code == "" {
		return nil, errors.New("missing code")
	}
	redirectURI := p.GetRedirectURI(host)
	s, err = p.provider.Redeem(ctx, redirectURI, code)
	if err != nil {
		return
	}

	if s.Email == "" {
		s.Email, err = p.provider.GetEmailAddress(ctx, s)
	}

	if s.PreferredUsername == "" {
		s.PreferredUsername, err = p.provider.GetPreferredUsername(ctx, s)
		if err != nil && err.Error() == "not implemented" {
			err = nil
		}
	}

	if s.User == "" {
		s.User, err = p.provider.GetUserName(ctx, s)
		if err != nil && err.Error() == "not implemented" {
			err = nil
		}
	}
	return
}

// MakeCSRFCookie creates a cookie for CSRF
func (p *OAuthProxy) MakeCSRFCookie(req *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	return p.makeCookie(req, p.CSRFCookieName, value, expiration, now)
}

func (p *OAuthProxy) makeCookie(req *http.Request, name string, value string, expiration time.Duration, now time.Time) *http.Cookie {
	cookieDomain := cookies.GetCookieDomain(req, p.CookieDomains)

	if cookieDomain != "" {
		domain := cookies.GetRequestHost(req)
		if h, _, err := net.SplitHostPort(domain); err == nil {
			domain = h
		}
		if !strings.HasSuffix(domain, cookieDomain) {
			logger.Printf("Warning: request host is %q but using configured cookie domain of %q", domain, cookieDomain)
		}
	}

	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     p.CookiePath,
		Domain:   cookieDomain,
		HttpOnly: p.CookieHTTPOnly,
		Secure:   p.CookieSecure,
		Expires:  now.Add(expiration),
		SameSite: cookies.ParseSameSite(p.CookieSameSite),
	}
}

// ClearCSRFCookie creates a cookie to unset the CSRF cookie stored in the user's
// session
func (p *OAuthProxy) ClearCSRFCookie(rw http.ResponseWriter, req *http.Request) {
	http.SetCookie(rw, p.MakeCSRFCookie(req, "", time.Hour*-1, time.Now()))
}

// SetCSRFCookie adds a CSRF cookie to the response
func (p *OAuthProxy) SetCSRFCookie(rw http.ResponseWriter, req *http.Request, val string) {
	http.SetCookie(rw, p.MakeCSRFCookie(req, val, p.CookieExpire, time.Now()))
}

// ClearSessionCookie creates a cookie to unset the user's authentication cookie
// stored in the user's session
func (p *OAuthProxy) ClearSessionCookie(rw http.ResponseWriter, req *http.Request) error {
	return p.sessionStore.Clear(rw, req)
}

// LoadCookiedSession reads the user's authentication details from the request
func (p *OAuthProxy) LoadCookiedSession(req *http.Request) (*sessionsapi.SessionState, error) {
	return p.sessionStore.Load(req)
}

// SaveSession creates a new session cookie value and sets this on the response
func (p *OAuthProxy) SaveSession(rw http.ResponseWriter, req *http.Request, s *sessionsapi.SessionState) error {
	return p.sessionStore.Save(rw, req, s)
}

// RobotsTxt disallows scraping pages from the OAuthProxy
func (p *OAuthProxy) RobotsTxt(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusOK)
	fmt.Fprintf(rw, "User-agent: *\nDisallow: /")
}

// ErrorPage writes an error response
func (p *OAuthProxy) ErrorPage(rw http.ResponseWriter, code int, title string, message string) {
	rw.WriteHeader(code)
	t := struct {
		Title       string
		Message     string
		ProxyPrefix string
	}{
		Title:       fmt.Sprintf("%d %s", code, title),
		Message:     message,
		ProxyPrefix: p.ProxyPrefix,
	}
	p.templates.ExecuteTemplate(rw, "error.html", t)
}

// SignInPage writes the sing in template to the response
func (p *OAuthProxy) SignInPage(rw http.ResponseWriter, req *http.Request, code int) {
	prepareNoCache(rw)
	p.ClearSessionCookie(rw, req)
	rw.WriteHeader(code)

	redirectURL, err := p.GetRedirect(req)
	if err != nil {
		logger.Printf("Error obtaining redirect: %s", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}

	if redirectURL == p.SignInPath {
		redirectURL = "/"
	}

	t := struct {
		ProviderName  string
		SignInMessage template.HTML
		CustomLogin   bool
		Redirect      string
		Version       string
		ProxyPrefix   string
		Footer        template.HTML
	}{
		ProviderName:  p.provider.Data().ProviderName,
		SignInMessage: template.HTML(p.SignInMessage),
		CustomLogin:   p.displayHtpasswdForm,
		Redirect:      redirectURL,
		Version:       VERSION,
		ProxyPrefix:   p.ProxyPrefix,
		Footer:        template.HTML(p.Footer),
	}
	if p.providerNameOverride != "" {
		t.ProviderName = p.providerNameOverride
	}
	p.templates.ExecuteTemplate(rw, "sign_in.html", t)
}

// ManualSignIn handles basic auth logins to the proxy
func (p *OAuthProxy) ManualSignIn(rw http.ResponseWriter, req *http.Request) (string, bool) {
	if req.Method != "POST" || p.basicAuthValidator == nil {
		return "", false
	}
	user := req.FormValue("username")
	passwd := req.FormValue("password")
	if user == "" {
		return "", false
	}
	// check auth
	if p.basicAuthValidator.Validate(user, passwd) {
		logger.PrintAuthf(user, req, logger.AuthSuccess, "Authenticated via HtpasswdFile")
		return user, true
	}
	logger.PrintAuthf(user, req, logger.AuthFailure, "Invalid authentication via HtpasswdFile")
	return "", false
}

// GetRedirect reads the query parameter to get the URL to redirect clients to
// once authenticated with the OAuthProxy
func (p *OAuthProxy) GetRedirect(req *http.Request) (redirect string, err error) {
	err = req.ParseForm()
	if err != nil {
		return
	}

	redirect = req.Header.Get("X-Auth-Request-Redirect")
	if req.Form.Get("rd") != "" {
		redirect = req.Form.Get("rd")
	}
	if !p.IsValidRedirect(redirect) {
		// Use RequestURI to preserve ?query
		redirect = req.URL.RequestURI()
		if strings.HasPrefix(redirect, p.ProxyPrefix) {
			redirect = "/"
		}
	}

	return
}

// splitHostPort separates host and port. If the port is not valid, it returns
// the entire input as host, and it doesn't check the validity of the host.
// Unlike net.SplitHostPort, but per RFC 3986, it requires ports to be numeric.
// *** taken from net/url, modified validOptionalPort() to accept ":*"
func splitHostPort(hostport string) (host, port string) {
	host = hostport

	colon := strings.LastIndexByte(host, ':')
	if colon != -1 && validOptionalPort(host[colon:]) {
		host, port = host[:colon], host[colon+1:]
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	return
}

// validOptionalPort reports whether port is either an empty string
// or matches /^:\d*$/
// *** taken from net/url, modified to accept ":*"
func validOptionalPort(port string) bool {
	if port == "" || port == ":*" {
		return true
	}
	if port[0] != ':' {
		return false
	}
	for _, b := range port[1:] {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// IsValidRedirect checks whether the redirect URL is whitelisted
func (p *OAuthProxy) IsValidRedirect(redirect string) bool {
	switch {
	case redirect == "":
		// The user didn't specify a redirect, should fallback to `/`
		return false
	case strings.HasPrefix(redirect, "/") && !strings.HasPrefix(redirect, "//") && !invalidRedirectRegex.MatchString(redirect):
		return true
	case strings.HasPrefix(redirect, "http://") || strings.HasPrefix(redirect, "https://"):
		redirectURL, err := url.Parse(redirect)
		if err != nil {
			logger.Printf("Rejecting invalid redirect %q: scheme unsupported or missing", redirect)
			return false
		}
		redirectHostname := redirectURL.Hostname()

		for _, domain := range p.whitelistDomains {
			domainHostname, domainPort := splitHostPort(strings.TrimLeft(domain, "."))
			if domainHostname == "" {
				continue
			}

			if (redirectHostname == domainHostname) || (strings.HasPrefix(domain, ".") && strings.HasSuffix(redirectHostname, domainHostname)) {
				// the domain names match, now validate the ports
				// if the whitelisted domain's port is '*', allow all ports
				// if the whitelisted domain contains a specific port, only allow that port
				// if the whitelisted domain doesn't contain a port at all, only allow empty redirect ports ie http and https
				redirectPort := redirectURL.Port()
				if (domainPort == "*") ||
					(domainPort == redirectPort) ||
					(domainPort == "" && redirectPort == "") {
					return true
				}
			}
		}

		logger.Printf("Rejecting invalid redirect %q: domain / port not in whitelist", redirect)
		return false
	default:
		logger.Printf("Rejecting invalid redirect %q: not an absolute or relative URL", redirect)
		return false
	}
}

// IsWhitelistedRequest is used to check if auth should be skipped for this request
func (p *OAuthProxy) IsWhitelistedRequest(req *http.Request) bool {
	isPreflightRequestAllowed := p.skipAuthPreflight && req.Method == "OPTIONS"
	return isPreflightRequestAllowed || p.IsWhitelistedPath(req.URL.Path) || p.IsTrustedIP(req)
}

// IsWhitelistedPath is used to check if the request path is allowed without auth
func (p *OAuthProxy) IsWhitelistedPath(path string) bool {
	for _, u := range p.compiledRegex {
		if u.MatchString(path) {
			return true
		}
	}
	return false
}

// See https://developers.google.com/web/fundamentals/performance/optimizing-content-efficiency/http-caching?hl=en
var noCacheHeaders = map[string]string{
	"Expires":         time.Unix(0, 0).Format(time.RFC1123),
	"Cache-Control":   "no-cache, no-store, must-revalidate, max-age=0",
	"X-Accel-Expires": "0", // https://www.nginx.com/resources/wiki/start/topics/examples/x-accel/
}

// prepareNoCache prepares headers for preventing browser caching.
func prepareNoCache(w http.ResponseWriter) {
	// Set NoCache headers
	for k, v := range noCacheHeaders {
		w.Header().Set(k, v)
	}
}

// IsTrustedIP is used to check if a request comes from a trusted client IP address.
func (p *OAuthProxy) IsTrustedIP(req *http.Request) bool {
	if p.trustedIPs == nil {
		return false
	}

	remoteAddr, err := ip.GetClientIP(p.realClientIPParser, req)
	if err != nil {
		logger.Printf("Error obtaining real IP for trusted IP list: %v", err)
		// Possibly spoofed X-Real-IP header
		return false
	}

	if remoteAddr == nil {
		return false
	}

	return p.trustedIPs.Has(remoteAddr)
}

func (p *OAuthProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != p.AuthOnlyPath && strings.HasPrefix(req.URL.Path, p.ProxyPrefix) {
		prepareNoCache(rw)
	}

	switch path := req.URL.Path; {
	case path == p.RobotsPath:
		p.RobotsTxt(rw)
	case p.IsWhitelistedRequest(req):
		p.SkipAuthProxy(rw, req)
	case path == p.SignInPath:
		p.SignIn(rw, req)
	case path == p.SignOutPath:
		p.SignOut(rw, req)
	case path == p.OAuthStartPath:
		p.OAuthStart(rw, req)
	case path == p.OAuthCallbackPath:
		p.OAuthCallback(rw, req)
	case path == p.AuthOnlyPath:
		p.AuthenticateOnly(rw, req)
	case path == p.UserInfoPath:
		p.UserInfo(rw, req)
	default:
		p.Proxy(rw, req)
	}
}

// SignIn serves a page prompting users to sign in
func (p *OAuthProxy) SignIn(rw http.ResponseWriter, req *http.Request) {
	redirect, err := p.GetRedirect(req)
	if err != nil {
		logger.Printf("Error obtaining redirect: %s", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}

	user, ok := p.ManualSignIn(rw, req)
	if ok {
		session := &sessionsapi.SessionState{User: user}
		p.SaveSession(rw, req, session)
		http.Redirect(rw, req, redirect, http.StatusFound)
	} else {
		if p.SkipProviderButton {
			p.OAuthStart(rw, req)
		} else {
			p.SignInPage(rw, req, http.StatusOK)
		}
	}
}

//UserInfo endpoint outputs session email and preferred username in JSON format
func (p *OAuthProxy) UserInfo(rw http.ResponseWriter, req *http.Request) {

	session, err := p.getAuthenticatedSession(rw, req)
	if err != nil {
		http.Error(rw, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	userInfo := struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferredUsername,omitempty"`
	}{
		Email:             session.Email,
		PreferredUsername: session.PreferredUsername,
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	json.NewEncoder(rw).Encode(userInfo)
}

// SignOut sends a response to clear the authentication cookie
func (p *OAuthProxy) SignOut(rw http.ResponseWriter, req *http.Request) {
	redirect, err := p.GetRedirect(req)
	if err != nil {
		logger.Printf("Error obtaining redirect: %s", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	p.ClearSessionCookie(rw, req)
	http.Redirect(rw, req, redirect, http.StatusFound)
}

// OAuthStart starts the OAuth2 authentication flow
func (p *OAuthProxy) OAuthStart(rw http.ResponseWriter, req *http.Request) {
	prepareNoCache(rw)
	nonce, err := encryption.Nonce()
	if err != nil {
		logger.Printf("Error obtaining nonce: %s", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	p.SetCSRFCookie(rw, req, nonce)
	redirect, err := p.GetRedirect(req)
	if err != nil {
		logger.Printf("Error obtaining redirect: %s", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	redirectURI := p.GetRedirectURI(req.Host)
	http.Redirect(rw, req, p.provider.GetLoginURL(redirectURI, fmt.Sprintf("%v:%v", nonce, redirect)), http.StatusFound)
}

// OAuthCallback is the OAuth2 authentication flow callback that finishes the
// OAuth2 authentication flow
func (p *OAuthProxy) OAuthCallback(rw http.ResponseWriter, req *http.Request) {
	remoteAddr := ip.GetClientString(p.realClientIPParser, req, true)

	// finish the oauth cycle
	err := req.ParseForm()
	if err != nil {
		logger.Printf("Error while parsing OAuth2 callback: %s" + err.Error())
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	errorString := req.Form.Get("error")
	if errorString != "" {
		logger.Printf("Error while parsing OAuth2 callback: %s ", errorString)
		p.ErrorPage(rw, 403, "Permission Denied", errorString)
		return
	}

	session, err := p.redeemCode(req.Context(), req.Host, req.Form.Get("code"))
	if err != nil {
		logger.Printf("Error redeeming code during OAuth2 callback: %s ", err.Error())
		p.ErrorPage(rw, 500, "Internal Error", "Internal Error")
		return
	}

	s := strings.SplitN(req.Form.Get("state"), ":", 2)
	if len(s) != 2 {
		logger.Printf("Error while parsing OAuth2 state: invalid length")
		p.ErrorPage(rw, 500, "Internal Error", "Invalid State")
		return
	}
	nonce := s[0]
	redirect := s[1]
	c, err := req.Cookie(p.CSRFCookieName)
	if err != nil {
		logger.PrintAuthf(session.Email, req, logger.AuthFailure, "Invalid authentication via OAuth2: unable too obtain CSRF cookie")
		p.ErrorPage(rw, 403, "Permission Denied", err.Error())
		return
	}
	p.ClearCSRFCookie(rw, req)
	if c.Value != nonce {
		logger.PrintAuthf(session.Email, req, logger.AuthFailure, "Invalid authentication via OAuth2: csrf token mismatch, potential attack")
		p.ErrorPage(rw, 403, "Permission Denied", "csrf failed")
		return
	}

	if !p.IsValidRedirect(redirect) {
		redirect = "/"
	}

	// set cookie, or deny
	if p.Validator(session.Email) && p.provider.ValidateGroup(session.Email) {
		logger.PrintAuthf(session.Email, req, logger.AuthSuccess, "Authenticated via OAuth2: %s", session)
		err := p.SaveSession(rw, req, session)
		if err != nil {
			logger.Printf("%s %s", remoteAddr, err)
			p.ErrorPage(rw, 500, "Internal Error", "Internal Error")
			return
		}
		http.Redirect(rw, req, redirect, http.StatusFound)
	} else {
		logger.PrintAuthf(session.Email, req, logger.AuthFailure, "Invalid authentication via OAuth2: unauthorized")
		p.ErrorPage(rw, 403, "Permission Denied", "Invalid Account")
	}
}

// AuthenticateOnly checks whether the user is currently logged in
func (p *OAuthProxy) AuthenticateOnly(rw http.ResponseWriter, req *http.Request) {
	session, err := p.getAuthenticatedSession(rw, req)
	if err != nil {
		http.Error(rw, "unauthorized request", http.StatusUnauthorized)
		return
	}

	// we are authenticated
	p.addHeadersForProxying(rw, req, session)
	rw.WriteHeader(http.StatusAccepted)
}

// SkipAuthProxy proxies whitelisted requests and skips authentication
func (p *OAuthProxy) SkipAuthProxy(rw http.ResponseWriter, req *http.Request) {
	if p.skipAuthStripHeaders {
		p.stripAuthHeaders(req)
	}
	p.serveMux.ServeHTTP(rw, req)
}

// Proxy proxies the user request if the user is authenticated else it prompts
// them to authenticate
func (p *OAuthProxy) Proxy(rw http.ResponseWriter, req *http.Request) {
	session, err := p.getAuthenticatedSession(rw, req)
	switch err {
	case nil:
		// we are authenticated
		p.addHeadersForProxying(rw, req, session)
		p.serveMux.ServeHTTP(rw, req)

	case ErrNeedsLogin:
		// we need to send the user to a login screen
		if isAjax(req) {
			// no point redirecting an AJAX request
			p.ErrorJSON(rw, http.StatusUnauthorized)
			return
		}

		if p.SkipProviderButton {
			p.OAuthStart(rw, req)
		} else {
			p.SignInPage(rw, req, http.StatusForbidden)
		}

	default:
		// unknown error
		logger.Printf("Unexpected internal error: %s", err)
		p.ErrorPage(rw, http.StatusInternalServerError,
			"Internal Error", "Internal Error")
	}

}

// getAuthenticatedSession checks whether a user is authenticated and returns a session object and nil error if so
// Returns nil, ErrNeedsLogin if user needs to login.
// Set-Cookie headers may be set on the response as a side-effect of calling this method.
func (p *OAuthProxy) getAuthenticatedSession(rw http.ResponseWriter, req *http.Request) (*sessionsapi.SessionState, error) {
	var session *sessionsapi.SessionState

	getSession := p.sessionChain.Then(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		session = middleware.GetRequestScope(req).Session
	}))
	getSession.ServeHTTP(rw, req)

	if session == nil {
		return nil, ErrNeedsLogin
	}

	if session != nil && session.Email != "" && !p.Validator(session.Email) {
		logger.Printf(session.Email, req, logger.AuthFailure, "Invalid authentication via session: removing session %s", session)
		// Invalid session, clear it
		p.ClearSessionCookie(rw, req)
		return nil, ErrNeedsLogin
	}

	return session, nil
}

// addHeadersForProxying adds the appropriate headers the request / response for proxying
func (p *OAuthProxy) addHeadersForProxying(rw http.ResponseWriter, req *http.Request, session *sessionsapi.SessionState) {
	if p.PassBasicAuth {
		if p.PreferEmailToUser && session.Email != "" {
			req.SetBasicAuth(session.Email, p.BasicAuthPassword)
			req.Header["X-Forwarded-User"] = []string{session.Email}
			req.Header.Del("X-Forwarded-Email")
		} else {
			req.SetBasicAuth(session.User, p.BasicAuthPassword)
			req.Header["X-Forwarded-User"] = []string{session.User}
			if session.Email != "" {
				req.Header["X-Forwarded-Email"] = []string{session.Email}
			} else {
				req.Header.Del("X-Forwarded-Email")
			}
		}
		if session.PreferredUsername != "" {
			req.Header["X-Forwarded-Preferred-Username"] = []string{session.PreferredUsername}
		} else {
			req.Header.Del("X-Forwarded-Preferred-Username")
		}
	}

	if p.PassUserHeaders {
		if p.PreferEmailToUser && session.Email != "" {
			req.Header["X-Forwarded-User"] = []string{session.Email}
			req.Header.Del("X-Forwarded-Email")
		} else {
			req.Header["X-Forwarded-User"] = []string{session.User}
			if session.Email != "" {
				req.Header["X-Forwarded-Email"] = []string{session.Email}
			} else {
				req.Header.Del("X-Forwarded-Email")
			}
		}

		if session.PreferredUsername != "" {
			req.Header["X-Forwarded-Preferred-Username"] = []string{session.PreferredUsername}
		} else {
			req.Header.Del("X-Forwarded-Preferred-Username")
		}
	}

	if p.SetXAuthRequest {
		rw.Header().Set("X-Auth-Request-User", session.User)
		if session.Email != "" {
			rw.Header().Set("X-Auth-Request-Email", session.Email)
		} else {
			rw.Header().Del("X-Auth-Request-Email")
		}
		if session.PreferredUsername != "" {
			rw.Header().Set("X-Auth-Request-Preferred-Username", session.PreferredUsername)
		} else {
			rw.Header().Del("X-Auth-Request-Preferred-Username")
		}

		if p.PassAccessToken {
			if session.AccessToken != "" {
				rw.Header().Set("X-Auth-Request-Access-Token", session.AccessToken)
			} else {
				rw.Header().Del("X-Auth-Request-Access-Token")
			}
		}
	}
	
	// For websockets we verify using a cookie as headers cannot be passed in the upgrade request
	http.SetCookie(rw, p.makeCookie(req, "X-Authorization", session.IDToken, p.CookieExpire, time.Now()))

	if p.PassAccessToken {
		if session.AccessToken != "" {
			req.Header["X-Forwarded-Access-Token"] = []string{session.AccessToken}
		} else {
			req.Header.Del("X-Forwarded-Access-Token")
		}
	}

	if p.PassAuthorization {
		if session.IDToken != "" {
			req.Header["Authorization"] = []string{fmt.Sprintf("Bearer %s", session.IDToken)}
		} else {
			req.Header.Del("Authorization")
		}
	}
	if p.SetBasicAuth {
		switch {
		case p.PreferEmailToUser && session.Email != "":
			authVal := b64.StdEncoding.EncodeToString([]byte(session.Email + ":" + p.BasicAuthPassword))
			rw.Header().Set("Authorization", "Basic "+authVal)
		case session.User != "":
			authVal := b64.StdEncoding.EncodeToString([]byte(session.User + ":" + p.BasicAuthPassword))
			rw.Header().Set("Authorization", "Basic "+authVal)
		default:
			rw.Header().Del("Authorization")
		}
	}
	if p.SetAuthorization {
		if session.IDToken != "" {
			rw.Header().Set("Authorization", fmt.Sprintf("Bearer %s", session.IDToken))
		} else {
			rw.Header().Del("Authorization")
		}
	}

	if session.Email == "" {
		rw.Header().Set("GAP-Auth", session.User)
	} else {
		rw.Header().Set("GAP-Auth", session.Email)
	}
}

// stripAuthHeaders removes Auth headers for whitelisted routes from skipAuthRegex
func (p *OAuthProxy) stripAuthHeaders(req *http.Request) {
	if p.PassBasicAuth {
		req.Header.Del("X-Forwarded-User")
		req.Header.Del("X-Forwarded-Email")
		req.Header.Del("X-Forwarded-Preferred-Username")
		req.Header.Del("Authorization")
	}

	if p.PassUserHeaders {
		req.Header.Del("X-Forwarded-User")
		req.Header.Del("X-Forwarded-Email")
		req.Header.Del("X-Forwarded-Preferred-Username")
	}

	if p.PassAccessToken {
		req.Header.Del("X-Forwarded-Access-Token")
	}

	if p.PassAuthorization {
		req.Header.Del("Authorization")
	}
}

// isAjax checks if a request is an ajax request
func isAjax(req *http.Request) bool {
	acceptValues := req.Header.Values("Accept")
	const ajaxReq = applicationJSON
	for _, v := range acceptValues {
		if v == ajaxReq {
			return true
		}
	}
	return false
}

// ErrorJSON returns the error code with an application/json mime type
func (p *OAuthProxy) ErrorJSON(rw http.ResponseWriter, code int) {
	rw.Header().Set("Content-Type", applicationJSON)
	rw.WriteHeader(code)
}
