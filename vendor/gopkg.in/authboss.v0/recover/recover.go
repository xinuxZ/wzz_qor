// Package recover implements password reset via e-mail.
package recover

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/authboss.v0"
	"gopkg.in/authboss.v0/internal/response"
)

// Storage constants
const (
	StoreRecoverToken       = "recover_token"
	StoreRecoverTokenExpiry = "recover_token_expiry"
)

const (
	formValueToken = "token"
)

const (
	methodGET  = "GET"
	methodPOST = "POST"

	tplLogin           = "login.html.tpl"
	tplRecover         = "recover.html.tpl"
	tplRecoverComplete = "recover_complete.html.tpl"
	tplInitHTMLEmail   = "recover_email.html.tpl"
	tplInitTextEmail   = "recover_email.txt.tpl"

	recoverInitiateSuccessFlash = "An email has been sent with further instructions on how to reset your password"
	recoverTokenExpiredFlash    = "Account recovery request has expired. Please try again."
	recoverFailedErrorFlash     = "Account recovery has failed. Please contact tech support."
)

var errRecoveryTokenExpired = errors.New("recovery token expired")

// RecoverStorer must be implemented in order to satisfy the recover module's
// storage requirements.
type RecoverStorer interface {
	authboss.Storer
	// RecoverUser looks a user up by a recover token. See recover module for
	// attribute names. If the key is not found in the data store,
	// simply return nil, ErrUserNotFound.
	RecoverUser(recoverToken string) (interface{}, error)
}

func init() {
	m := &Recover{}
	authboss.RegisterModule("recover", m)
}

// Recover module
type Recover struct {
	*authboss.Authboss
	templates          response.Templates
	emailHTMLTemplates response.Templates
	emailTextTemplates response.Templates
}

// Initialize module
func (r *Recover) Initialize(ab *authboss.Authboss) (err error) {
	r.Authboss = ab

	if r.Storer != nil {
		if _, ok := r.Storer.(RecoverStorer); !ok {
			return errors.New("recover: RecoverStorer required for recover functionality")
		}
	} else if r.StoreMaker == nil {
		return errors.New("recover: Need a RecoverStorer")
	}

	if len(r.XSRFName) == 0 {
		return errors.New("auth: XSRFName must be set")
	}

	if r.XSRFMaker == nil {
		return errors.New("auth: XSRFMaker must be defined")
	}

	r.emailHTMLTemplates, err = response.LoadTemplates(r.Authboss, r.LayoutHTMLEmail, r.ViewsPath, tplInitHTMLEmail)
	if err != nil {
		return err
	}
	r.emailTextTemplates, err = response.LoadTemplates(r.Authboss, r.LayoutTextEmail, r.ViewsPath, tplInitTextEmail)
	if err != nil {
		return err
	}

	return nil
}

// Routes for module
func (r *Recover) Routes() authboss.RouteTable {
	return authboss.RouteTable{
		"/recover":          r.startHandlerFunc,
		"/recover/complete": r.completeHandlerFunc,
	}
}

// Storage requirements
func (r *Recover) Storage() authboss.StorageOptions {
	return authboss.StorageOptions{
		r.PrimaryID:             authboss.String,
		authboss.StoreEmail:     authboss.String,
		authboss.StorePassword:  authboss.String,
		StoreRecoverToken:       authboss.String,
		StoreRecoverTokenExpiry: authboss.String,
	}
}

func (rec *Recover) startHandlerFunc(ctx *authboss.Context, w http.ResponseWriter, r *http.Request) (err error) {
	rec.templates, err = response.LazyLoadTemplates(w, r, rec.Authboss, tplRecover)
	if err != nil {
		return err
	}
	switch r.Method {
	case methodGET:
		data := authboss.NewHTMLData(
			"primaryID", rec.PrimaryID,
			"primaryIDValue", "",
			"confirmPrimaryIDValue", "",
		)
		return rec.templates.Render(ctx, w, r, tplRecover, data)
	case methodPOST:
		primaryID := r.FormValue(rec.PrimaryID)
		confirmPrimaryID := r.FormValue(fmt.Sprintf("confirm_%s", rec.PrimaryID))

		errData := authboss.NewHTMLData(
			"primaryID", rec.PrimaryID,
			"primaryIDValue", primaryID,
			"confirmPrimaryIDValue", confirmPrimaryID,
		)

		policies := authboss.FilterValidators(rec.Policies, rec.PrimaryID)
		if validationErrs := authboss.Validate(r, policies, rec.PrimaryID, authboss.ConfirmPrefix+rec.PrimaryID).Map(); len(validationErrs) > 0 {
			errData.MergeKV("errs", validationErrs)
			return rec.templates.Render(ctx, w, r, tplRecover, errData)
		}

		// redirect to login when user not found to prevent username sniffing
		if err := ctx.LoadUser(primaryID); err == authboss.ErrUserNotFound {
			return authboss.ErrAndRedirect{Err: err, Location: rec.RecoverOKPath, FlashSuccess: recoverInitiateSuccessFlash}
		} else if err != nil {
			return err
		}

		email, err := ctx.User.StringErr(authboss.StoreEmail)
		if err != nil {
			return err
		}

		encodedToken, encodedChecksum, err := newToken()
		if err != nil {
			return err
		}

		ctx.User[StoreRecoverToken] = encodedChecksum
		ctx.User[StoreRecoverTokenExpiry] = time.Now().Add(rec.RecoverTokenDuration)

		if err := ctx.SaveUser(); err != nil {
			return err
		}

		goRecoverEmail(rec, ctx, email, encodedToken)

		ctx.SessionStorer.Put(authboss.FlashSuccessKey, recoverInitiateSuccessFlash)
		response.Redirect(ctx, w, r, rec.RecoverOKPath, "", "", true)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}

	return nil
}

func newToken() (encodedToken, encodedChecksum string, err error) {
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return "", "", err
	}
	sum := md5.Sum(token)

	return base64.URLEncoding.EncodeToString(token), base64.StdEncoding.EncodeToString(sum[:]), nil
}

var goRecoverEmail = func(r *Recover, ctx *authboss.Context, to, encodedToken string) {
	if ctx.MailMaker != nil {
		r.sendRecoverEmail(ctx, to, encodedToken)
	} else {
		go r.sendRecoverEmail(ctx, to, encodedToken)
	}
}

func (r *Recover) sendRecoverEmail(ctx *authboss.Context, to, encodedToken string) {
	p := path.Join(r.MountPath, "recover/complete")
	query := url.Values{formValueToken: []string{encodedToken}}
	url := fmt.Sprintf("%s%s?%s", r.RootURL, p, query.Encode())

	email := authboss.Email{
		To:      []string{to},
		From:    r.EmailFrom,
		Subject: r.EmailSubjectPrefix + "Password Reset",
	}

	if err := response.Email(ctx.Mailer, email, r.emailHTMLTemplates, tplInitHTMLEmail, r.emailTextTemplates, tplInitTextEmail, url); err != nil {
		fmt.Fprintln(ctx.LogWriter, "recover: failed to send recover email:", err)
	}
}

func (r *Recover) completeHandlerFunc(ctx *authboss.Context, w http.ResponseWriter, req *http.Request) (err error) {
	r.templates, err = response.LazyLoadTemplates(w, req, r.Authboss, tplRecoverComplete)
	if err != nil {
		return err
	}
	switch req.Method {
	case methodGET:
		_, err = verifyToken(ctx, req)
		if err == errRecoveryTokenExpired {
			return authboss.ErrAndRedirect{Err: err, Location: "/recover", FlashError: recoverTokenExpiredFlash}
		} else if err != nil {
			return authboss.ErrAndRedirect{Err: err, Location: "/"}
		}

		token := req.FormValue(formValueToken)
		data := authboss.NewHTMLData(formValueToken, token)
		return r.templates.Render(ctx, w, req, tplRecoverComplete, data)
	case methodPOST:
		token := req.FormValue(formValueToken)
		if len(token) == 0 {
			return authboss.ClientDataErr{Name: formValueToken}
		}

		password := req.FormValue(authboss.StorePassword)
		//confirmPassword, _ := ctx.FirstPostFormValue("confirmPassword")

		policies := authboss.FilterValidators(r.Policies, authboss.StorePassword)
		if validationErrs := authboss.Validate(req, policies, authboss.StorePassword, authboss.ConfirmPrefix+authboss.StorePassword).Map(); len(validationErrs) > 0 {
			data := authboss.NewHTMLData(
				formValueToken, token,
				"errs", validationErrs,
			)
			return r.templates.Render(ctx, w, req, tplRecoverComplete, data)
		}

		if ctx.User, err = verifyToken(ctx, req); err != nil {
			return err
		}

		encryptedPassword, err := bcrypt.GenerateFromPassword([]byte(password), r.BCryptCost)
		if err != nil {
			return err
		}

		ctx.User[authboss.StorePassword] = string(encryptedPassword)
		ctx.User[StoreRecoverToken] = ""
		var nullTime time.Time
		ctx.User[StoreRecoverTokenExpiry] = nullTime

		primaryID, err := ctx.User.StringErr(r.PrimaryID)
		if err != nil {
			return err
		}

		if err := ctx.SaveUser(); err != nil {
			return err
		}

		if err := r.Callbacks.FireAfter(authboss.EventPasswordReset, ctx); err != nil {
			return err
		}

		ctx.SessionStorer.Put(authboss.SessionKey, primaryID)
		response.Redirect(ctx, w, req, r.AuthLoginOKPath, "", "", true)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}

	return nil
}

// verifyToken expects a base64.URLEncoded token.
func verifyToken(ctx *authboss.Context, r *http.Request) (attrs authboss.Attributes, err error) {
	token := r.FormValue(formValueToken)
	if len(token) == 0 {
		return nil, authboss.ClientDataErr{Name: token}
	}

	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}

	sum := md5.Sum(decoded)
	storer := ctx.Storer.(RecoverStorer)

	userInter, err := storer.RecoverUser(base64.StdEncoding.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}

	attrs = authboss.Unbind(userInter)

	expiry, ok := attrs.DateTime(StoreRecoverTokenExpiry)
	if !ok || time.Now().After(expiry) {
		return nil, errRecoveryTokenExpired
	}

	return attrs, nil
}
