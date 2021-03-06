package tokens

import (
	"context"
	"fmt"

	"github.com/rancher/rancher/pkg/randomtoken"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// TODO Cleanup error logging. If error is being returned, use errors.wrap to return and dont log here

const (
	userPrincipalIndex = "authn.management.cattle.io/user-principal-index"
	UserIDLabel        = "authn.management.cattle.io/token-userId"
	tokenKeyIndex      = "authn.management.cattle.io/token-key-index"
)

type tokenAPIServer struct {
	ctx          context.Context
	tokensClient v3.TokenInterface
	userIndexer  cache.Indexer
	tokenIndexer cache.Indexer
}

var tokenServer *tokenAPIServer

func userPrincipalIndexer(obj interface{}) ([]string, error) {
	user, ok := obj.(*v3.User)
	if !ok {
		return []string{}, nil
	}

	return user.PrincipalIDs, nil
}

func tokenKeyIndexer(obj interface{}) ([]string, error) {
	token, ok := obj.(*v3.Token)
	if !ok {
		return []string{}, nil
	}

	return []string{token.Token}, nil
}

func NewTokenAPIServer(ctx context.Context, apiContext *config.ScaledContext) error {
	if tokenServer != nil {
		return nil
	}

	if apiContext == nil {
		return fmt.Errorf("failed to build tokenAPIHandler, nil ManagementContext")
	}

	informer := apiContext.Management.Users("").Controller().Informer()
	informer.AddIndexers(map[string]cache.IndexFunc{userPrincipalIndex: userPrincipalIndexer})
	tokenInformer := apiContext.Management.Tokens("").Controller().Informer()
	tokenInformer.AddIndexers(map[string]cache.IndexFunc{tokenKeyIndex: tokenKeyIndexer})
	tokenServer = &tokenAPIServer{
		ctx:          ctx,
		tokensClient: apiContext.Management.Tokens(""),
		userIndexer:  informer.GetIndexer(),
		tokenIndexer: tokenInformer.GetIndexer(),
	}

	return nil
}

//CreateDerivedToken will create a jwt token for the authenticated user
func (s *tokenAPIServer) createDerivedToken(jsonInput v3.Token, tokenAuthValue string) (v3.Token, int, error) {

	logrus.Debug("Create Derived Token Invoked")

	token, _, err := s.getK8sTokenCR(tokenAuthValue)
	if err != nil {
		return v3.Token{}, 401, err
	}

	k8sToken := &v3.Token{
		UserPrincipal:   token.UserPrincipal,
		GroupPrincipals: token.GroupPrincipals,
		IsDerived:       true,
		TTLMillis:       jsonInput.TTLMillis,
		UserID:          token.UserID,
		AuthProvider:    token.AuthProvider,
		ProviderInfo:    token.ProviderInfo,
		Description:     jsonInput.Description,
	}
	rToken, err := s.createK8sTokenCR(k8sToken)

	return rToken, 0, err

}

func (s *tokenAPIServer) createK8sTokenCR(k8sToken *v3.Token) (v3.Token, error) {
	key, err := randomtoken.Generate()
	if err != nil {
		logrus.Errorf("Failed to generate token key: %v", err)
		return v3.Token{}, fmt.Errorf("failed to generate token key")
	}

	labels := make(map[string]string)
	labels[UserIDLabel] = k8sToken.UserID

	k8sToken.APIVersion = "management.cattle.io/v3"
	k8sToken.Kind = "Token"
	k8sToken.Token = key
	k8sToken.ObjectMeta = metav1.ObjectMeta{
		GenerateName: "token-",
		Labels:       labels,
	}
	createdToken, err := s.tokensClient.Create(k8sToken)

	if err != nil {
		return v3.Token{}, err
	}

	return *createdToken, nil
}

func (s *tokenAPIServer) updateK8sTokenCR(token *v3.Token) (*v3.Token, error) {
	return s.tokensClient.Update(token)
}

func (s *tokenAPIServer) getK8sTokenCR(tokenAuthValue string) (*v3.Token, int, error) {
	tokenName, tokenKey := SplitTokenParts(tokenAuthValue)

	lookupUsingClient := false

	objs, err := s.tokenIndexer.ByIndex(tokenKeyIndex, tokenKey)
	if err != nil {
		if apierrors.IsNotFound(err) {
			lookupUsingClient = true
		} else {
			return nil, 0, fmt.Errorf("failed to retrieve auth token from cache, error: %v", err)
		}
	} else if len(objs) == 0 {
		lookupUsingClient = true
	}

	storedToken := &v3.Token{}
	if lookupUsingClient {
		storedToken, err = s.tokensClient.Get(tokenName, metav1.GetOptions{})
		if err != nil {
			return nil, 404, fmt.Errorf("failed to retrieve auth token, error: %#v", err)
		}
	} else {
		storedToken = objs[0].(*v3.Token)
	}

	if storedToken.Token != tokenKey || storedToken.ObjectMeta.Name != tokenName {
		return nil, 0, fmt.Errorf("Invalid auth token value")
	}

	if IsExpired(*storedToken) {
		return storedToken, 410, fmt.Errorf("Auth Token has expired")
	}

	return storedToken, 0, nil
}

//GetTokens will list all(login and derived, and even expired) tokens of the authenticated user
func (s *tokenAPIServer) getTokens(tokenAuthValue string) ([]v3.Token, int, error) {
	logrus.Debug("LIST Tokens Invoked")
	tokens := make([]v3.Token, 0)

	storedToken, _, err := s.getK8sTokenCR(tokenAuthValue)
	if err != nil {
		return tokens, 401, err
	}

	userID := storedToken.UserID
	set := labels.Set(map[string]string{UserIDLabel: userID})
	tokenList, err := s.tokensClient.List(metav1.ListOptions{LabelSelector: set.AsSelector().String()})
	if err != nil {
		return tokens, 0, fmt.Errorf("error getting tokens for user: %v selector: %v  err: %v", userID, set.AsSelector().String(), err)
	}

	for _, t := range tokenList.Items {
		if IsExpired(t) {
			t.Expired = true
		}
		tokens = append(tokens, t)
	}
	return tokens, 0, nil
}

func (s *tokenAPIServer) deleteToken(tokenAuthValue string) (int, error) {
	logrus.Debug("DELETE Token Invoked")

	storedToken, status, err := s.getK8sTokenCR(tokenAuthValue)
	if err != nil {
		if status == 404 {
			return 0, nil
		} else if status != 410 {
			return 401, err
		}
	}

	return s.deleteTokenByName(storedToken.Name)
}

func (s *tokenAPIServer) deleteTokenByName(tokenName string) (int, error) {
	err := s.tokensClient.Delete(tokenName, &metav1.DeleteOptions{})
	if err != nil {
		if e2, ok := err.(*errors.StatusError); ok && e2.Status().Code == 404 {
			return 0, nil
		}
		return 500, fmt.Errorf("failed to delete token")
	}
	logrus.Debug("Deleted Token")
	return 0, nil
}

//getToken will get the token by ID
func (s *tokenAPIServer) getTokenByID(tokenAuthValue string, tokenID string) (v3.Token, int, error) {
	logrus.Debug("GET Token Invoked")
	token := &v3.Token{}

	storedToken, _, err := s.getK8sTokenCR(tokenAuthValue)
	if err != nil {
		return *token, 401, err
	}

	token, err = s.tokensClient.Get(tokenID, metav1.GetOptions{})
	if err != nil {
		return v3.Token{}, 404, err
	}

	if token.UserID != storedToken.UserID {
		return v3.Token{}, 404, fmt.Errorf("%v not found", tokenID)
	}

	if IsExpired(*token) {
		token.Expired = true
	}

	return *token, 0, nil
}
