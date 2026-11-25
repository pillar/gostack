package app

import (
	"crypto/rsa"
	"fmt"
	"os"

	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/commerce"
	"github.com/pillar/gostack/modules/jobs"
)

// Google Play and WeChat Pay commerce providers. Secrets are read from secret
// references at startup and never logged.

func newGoogleVerifier(a *App, c *config.GoogleIAPConfig) (*commerce.GoogleVerifier, error) {
	key, err := config.ResolveSecret(c.ServiceAccountRef)
	if err != nil {
		return nil, fmt.Errorf("game.commerce.google.service_account_ref: %w", err)
	}
	sa, err := commerce.NewGoogleServiceAccount([]byte(key), c.TokenURL)
	if err != nil {
		return nil, err
	}
	apps := map[string]commerce.GoogleApp{}
	for g, app := range c.Apps {
		apps[g] = commerce.GoogleApp{PackageName: app.PackageName, AllowTest: app.AllowTestPurchases, Acknowledge: app.Finish == "acknowledge"}
	}
	v := &commerce.GoogleVerifier{Apps: apps, API: &commerce.GooglePlayAPI{BaseURL: c.APIBaseURL, Tokens: sa}}
	if r := c.RTDN; r != nil {
		v.RTDN = &commerce.GoogleRTDN{Audience: r.Audience, ServiceAccountEmail: r.ServiceAccountEmail, Keys: &commerce.JWKSFetcher{URL: r.JWKSURL}}
	}
	return v, nil
}

func newWeChatPayVerifier(a *App, c *config.WeChatPayIAPConfig) (*commerce.WeChatPayVerifier, error) {
	pk, err := config.ResolveSecret(c.MerchantPrivateKeyRef)
	if err != nil {
		return nil, fmt.Errorf("game.commerce.wechatpay.merchant_private_key_ref: %w", err)
	}
	key, err := commerce.ParseRSAPrivateKey([]byte(pk))
	if err != nil {
		return nil, fmt.Errorf("game.commerce.wechatpay.merchant_private_key_ref: %w", err)
	}
	v3, err := config.ResolveSecret(c.APIv3KeyRef)
	if err != nil {
		return nil, fmt.Errorf("game.commerce.wechatpay.apiv3_key_ref: %w", err)
	}
	if len(v3) != 32 {
		return nil, fmt.Errorf("game.commerce.wechatpay.apiv3_key_ref: the APIv3 key must be 32 bytes")
	}
	keys := map[string]*rsa.PublicKey{}
	for serial, f := range c.PlatformKeys {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("game.commerce.wechatpay.platform_keys.%s: %w", serial, err)
		}
		if keys[serial], err = commerce.ParseWeChatPayPublicKey(b); err != nil {
			return nil, fmt.Errorf("game.commerce.wechatpay.platform_keys.%s: %w", serial, err)
		}
	}
	apps := map[string]commerce.WeChatPayApp{}
	for g, app := range c.Apps {
		apps[g] = commerce.WeChatPayApp{AppID: app.AppID, Prices: app.Prices, Description: app.Description}
	}
	sigs := &commerce.WeChatPaySignatures{Keys: keys, MaxSkew: c.MaxSkew}
	merchant := &commerce.WeChatPayMerchant{MchID: c.MchID, SerialNo: c.MerchantSerialNo, Key: key}
	return &commerce.WeChatPayVerifier{MchID: c.MchID, APIv3Key: []byte(v3), Signatures: sigs, Merchant: merchant,
		Client: &commerce.WeChatPayHTTPClient{BaseURL: c.APIBaseURL, Merchant: merchant, Verify: sigs},
		Apps:   apps, NotifyBaseURL: c.NotifyBaseURL, QueryAfter: c.QueryAfter, Log: a.Log}, nil
}

// moduleJobHandlers returns job handlers of enabled modules (Google Play
// acknowledge/consume).
func (a *App) moduleJobHandlers() map[string]jobs.Handler {
	out := map[string]jobs.Handler{}
	if a.Mod.Commerce != nil {
		for k, v := range a.Mod.Commerce.JobHandlers() {
			out[k] = v
		}
	}
	return out
}
