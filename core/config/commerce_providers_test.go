package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
)

func validGoogle() *config.GoogleIAPConfig {
	return &config.GoogleIAPConfig{ServiceAccountRef: "file:/run/secrets/play.json",
		Apps: map[string]config.GoogleAppConf{"g": {PackageName: "com.example.g"}},
		RTDN: &config.GoogleRTDNConf{Audience: "https://api.example.com/rtdn", ServiceAccountEmail: "push@p.iam.gserviceaccount.com"}}
}

func validWeChatPay() *config.WeChatPayIAPConfig {
	return &config.WeChatPayIAPConfig{MchID: "1900000001", MerchantSerialNo: "ABC", MerchantPrivateKeyRef: "env:WX_KEY", APIv3KeyRef: "env:WX_V3",
		PlatformKeys: map[string]string{"PUB_KEY_ID_1": "/etc/wx.pem"}, NotifyBaseURL: "https://api.example.com",
		Apps: map[string]config.WeChatPayAppConf{"g": {AppID: "wx1", Prices: map[string]int64{"gems": 600}}}}
}

func TestCommerceProviderValidation(t *testing.T) {
	c := config.Defaults()
	c.Game.Commerce.Google, c.Game.Commerce.WeChatPay = validGoogle(), validWeChatPay()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid provider config rejected: %v", err)
	}
	for name, mut := range map[string]func(*config.Config){
		"google literal secret": func(c *config.Config) { c.Game.Commerce.Google.ServiceAccountRef = "{json}" },
		"google no apps":        func(c *config.Config) { c.Game.Commerce.Google.Apps = nil },
		"google no package":     func(c *config.Config) { c.Game.Commerce.Google.Apps["g"] = config.GoogleAppConf{} },
		"google bad finish": func(c *config.Config) {
			c.Game.Commerce.Google.Apps["g"] = config.GoogleAppConf{PackageName: "p", Finish: "refund"}
		},
		"google test in prod": func(c *config.Config) {
			c.Environment = "production"
			c.Game.Commerce.Google.Apps["g"] = config.GoogleAppConf{PackageName: "p", AllowTestPurchases: true}
		},
		"google rtdn no audience": func(c *config.Config) { c.Game.Commerce.Google.RTDN.Audience = "" },
		"wechat literal key":      func(c *config.Config) { c.Game.Commerce.WeChatPay.MerchantPrivateKeyRef = "-----BEGIN" },
		"wechat literal v3":       func(c *config.Config) { c.Game.Commerce.WeChatPay.APIv3KeyRef = "0123" },
		"wechat no mch":           func(c *config.Config) { c.Game.Commerce.WeChatPay.MchID = "" },
		"wechat no platform keys": func(c *config.Config) { c.Game.Commerce.WeChatPay.PlatformKeys = nil },
		"wechat relative notify":  func(c *config.Config) { c.Game.Commerce.WeChatPay.NotifyBaseURL = "/cb" },
		"wechat negative query":   func(c *config.Config) { c.Game.Commerce.WeChatPay.QueryAfter = -time.Minute },
		"wechat query after ttl":  func(c *config.Config) { c.Game.Commerce.WeChatPay.QueryAfter = 3 * time.Hour },
		"wechat http notify prod": func(c *config.Config) {
			c.Environment = "production"
			c.Game.Commerce.WeChatPay.NotifyBaseURL = "http://api.example.com"
		},
		"wechat zero price": func(c *config.Config) {
			c.Game.Commerce.WeChatPay.Apps["g"] = config.WeChatPayAppConf{AppID: "wx1", Prices: map[string]int64{"gems": 0}}
		},
		"wechat no app id": func(c *config.Config) {
			c.Game.Commerce.WeChatPay.Apps["g"] = config.WeChatPayAppConf{Prices: map[string]int64{"gems": 1}}
		},
	} {
		c := config.Defaults()
		c.Game.Commerce.Google, c.Game.Commerce.WeChatPay = validGoogle(), validWeChatPay()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCommerceProviderYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(`game:
  commerce:
    google:
      service_account_ref: file:/run/secrets/play.json
      apps: { g: { package_name: com.example.g, finish: acknowledge } }
      rtdn: { audience: "https://api.example.com/rtdn", service_account_email: push@p.iam.gserviceaccount.com }
    wechatpay:
      mch_id: "1900000001"
      merchant_serial_no: ABC
      merchant_private_key_ref: env:WX_KEY
      apiv3_key_ref: env:WX_V3
      platform_keys: { PUB_KEY_ID_1: secrets/wx.pem }
      notify_base_url: https://api.example.com
      apps: { g: { app_id: wx1, prices: { gems_100: 600 } } }
`), 0o600)
	c, err := config.Load(p, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Game.Commerce.Google.Apps["g"].Finish != "acknowledge" || c.Game.Commerce.WeChatPay.Apps["g"].Prices["gems_100"] != 600 ||
		c.Game.Commerce.WeChatPay.PlatformKeys["PUB_KEY_ID_1"] != filepath.Join(dir, "secrets/wx.pem") {
		t.Fatalf("%+v %+v", c.Game.Commerce.Google, c.Game.Commerce.WeChatPay)
	}
}
