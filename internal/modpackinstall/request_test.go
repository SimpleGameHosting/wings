package modpackinstall

import "testing"

func valid() Request {
	return Request{
		InstallID:   "11111111-2222-3333-4444-555555555555",
		Kind:        KindModpack,
		DownloadURL: "http://cdn-stub/pack-plain.tar.gz",
	}
}

func TestRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *Request)
		wantErr bool
	}{
		{"valid modpack", func(r *Request) {}, false},
		{"valid version jar", func(r *Request) { r.Kind = KindVersion; r.VersionType = "paper"; r.ArchiveFormat = "jar" }, false},
		{"valid version archive", func(r *Request) { r.Kind = KindVersion; r.VersionType = "forge" }, false},
		{"missing install id", func(r *Request) { r.InstallID = "" }, true},
		{"non-uuid install id", func(r *Request) { r.InstallID = "not-a-uuid" }, true},
		{"unknown kind", func(r *Request) { r.Kind = "banana" }, true},
		{"unknown version type", func(r *Request) { r.Kind = KindVersion; r.VersionType = "quilt" }, true},
		{"version without type", func(r *Request) { r.Kind = KindVersion }, true},
		{"modpack with version type", func(r *Request) { r.VersionType = "paper" }, true},
		{"bad url scheme", func(r *Request) { r.DownloadURL = "ftp://x/y" }, true},
		{"explicit non-jar format rejected", func(r *Request) { r.ArchiveFormat = "tar.gz" }, true},
	}
	for _, tc := range cases {
		r := valid()
		tc.mutate(&r)
		err := r.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
