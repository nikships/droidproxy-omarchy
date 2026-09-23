package updater

// Feed is latest.json, published as a release asset on every release.
type Feed struct {
	Version     string           `json:"version"`
	Tag         string           `json:"tag"`
	PublishedAt string           `json:"publishedAt"`
	Notes       string           `json:"notes"`
	ReleaseURL  string           `json:"releaseUrl"`
	Assets      map[string]Asset `json:"assets"`
}

// Asset is one platform's downloadable artifact.
type Asset struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"` // base64 ed25519 over the raw tarball bytes
}

// PlatformAsset returns the feed asset for this machine, or nil.
func (f *Feed) PlatformAsset() *Asset {
	if a, ok := f.Assets[AssetPlatform()]; ok {
		return &a
	}
	return nil
}
