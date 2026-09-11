package webui

import "net/http"

func (u *UI) loadBranding(r *http.Request, base *Base) {
	var brand struct {
		BrandName  string `json:"brand_name"`
		FaviconURL string `json:"favicon_url"`
		LogoURL    string `json:"logo_url"`
	}
	if res := u.call(r, "", http.MethodGet, "/brand", nil, nil); res.Status == http.StatusOK {
		if err := res.Decode(&brand); err == nil {
			if brand.BrandName != "" {
				base.BrandName = brand.BrandName
			}
			if brand.FaviconURL != "" {
				base.FaviconURL = brand.FaviconURL
			}
			if brand.LogoURL != "" {
				base.LogoURL = brand.LogoURL
			}
		}
	}
}
