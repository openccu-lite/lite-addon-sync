package accounts

import (
	"strings"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	sa := `{"type":"service_account","client_email":"backup@proj.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"}`
	ok := []Spec{
		{Name: "Privat", Kind: KindOneDrive},
		{Name: "Privat", Kind: KindOneDrive, ClientID: "abc-123", ClientSecret: "s~ecret.1"},
		{Name: "Box", Kind: KindDropbox},
		{Name: "Haus", Kind: KindDrive, ClientID: "1234.apps.googleusercontent.com", ClientSecret: "GOCSPX-x"},
		{Name: "Haus", Kind: KindDrive, ServiceAccount: sa, FolderID: "1AbCdEfGhIjKlMn"},
		{Name: "B", Kind: KindS3, Bucket: "my-backups", AccessKey: "AKIAEXAMPLE", SecretKey: "abc/def+ghi"},
		{Name: "B", Kind: KindS3, Endpoint: "https://s3.example.org", Bucket: "my-backups", AccessKey: "k", SecretKey: "s", Region: "eu-central-1"},
		{Name: "NAS", Kind: KindWebDAV, URL: "https://cloud.example.org/remote.php/dav/files/me", Vendor: "nextcloud", User: "me", Password: "app pass word"},
	}
	for _, sp := range ok {
		if err := sp.Validate(); err != nil {
			t.Errorf("%+v: %v", sp, err)
		}
	}
	bad := map[string]Spec{
		"no name":             {Kind: KindOneDrive},
		"a name of two lines": {Name: "a\nb", Kind: KindOneDrive},
		"half a client":       {Name: "x", Kind: KindOneDrive, ClientID: "abc"},
		"drive without":       {Name: "x", Kind: KindDrive},
		"drive sa, no folder": {Name: "x", Kind: KindDrive, ServiceAccount: sa},
		"drive sa, no key":    {Name: "x", Kind: KindDrive, ServiceAccount: `{"type":"authorized_user"}`, FolderID: "1AbCdEfGh"},
		"a client as a flag":  {Name: "x", Kind: KindDropbox, ClientID: "--config=/etc/x", ClientSecret: "y z"},
		"s3 no bucket":        {Name: "x", Kind: KindS3, AccessKey: "k", SecretKey: "s"},
		"s3 bucket as a path": {Name: "x", Kind: KindS3, Bucket: "../up", AccessKey: "k", SecretKey: "s"},
		"s3 other, no host":   {Name: "x", Kind: KindS3, Provider: "Minio", Bucket: "bucket", AccessKey: "k", SecretKey: "s"},
		"s3 odd provider":     {Name: "x", Kind: KindS3, Provider: "Mine", Endpoint: "https://x.example", Bucket: "bucket", AccessKey: "k", SecretKey: "s"},
		"s3 secret as a flag": {Name: "x", Kind: KindS3, Bucket: "bucket", AccessKey: "k", SecretKey: "--password-command=x"},
		"webdav no scheme":    {Name: "x", Kind: KindWebDAV, URL: "cloud.example.org", User: "u", Password: "p"},
		"webdav file url":     {Name: "x", Kind: KindWebDAV, URL: "file:///etc", User: "u", Password: "p"},
		"webdav url login":    {Name: "x", Kind: KindWebDAV, URL: "https://u:p@cloud.example.org/", User: "u", Password: "p"},
		"webdav odd vendor":   {Name: "x", Kind: KindWebDAV, URL: "https://c.example/", Vendor: "sharepoint-ntlm", User: "u", Password: "p"},
		"webdav no password":  {Name: "x", Kind: KindWebDAV, URL: "https://c.example/", User: "u"},
		"an unknown kind":     {Name: "x", Kind: "sftp"},
	}
	for name, sp := range bad {
		if err := sp.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParamsAndDetail(t *testing.T) {
	sp := Spec{Name: "B", Kind: KindS3, Endpoint: "https://s3.example.org", Bucket: "my-backups", AccessKey: "k", SecretKey: "s"}
	_ = sp.Validate()
	typ, p, root, detail := sp.params("")
	if typ != "s3" || root != "my-backups" || p["provider"] != "Other" || p["env_auth"] != "false" || detail != "S3 · my-backups · s3.example.org" {
		t.Fatalf("%s %v %s %s", typ, p, root, detail)
	}
	// Google Drive with an own client sees only what rclone made; a service account the folder shared with it
	_, p, _, _ = Spec{Kind: KindDrive, ClientID: "c", ClientSecret: "s"}.params(`{"access_token":"a"}`)
	if p["scope"] != "drive.file" || p["token"] == "" {
		t.Fatalf("%v", p)
	}
	_, p, _, detail = Spec{Kind: KindDrive, ServiceAccount: `{"client_email":"b@p.iam"}`, FolderID: "F1"}.params("")
	if p["scope"] != "drive" || p["root_folder_id"] != "F1" || p["client_id"] != "" || !strings.HasSuffix(detail, "b@p.iam") {
		t.Fatalf("%v %s", p, detail)
	}
	if got := Command(Spec{Kind: KindDrive, ClientID: "id", ClientSecret: "sec"}); got != `rclone authorize "drive" "id" "sec"` {
		t.Errorf("%s", got)
	}
	if got := Command(Spec{Kind: KindOneDrive}); got != `rclone authorize "onedrive"` {
		t.Errorf("%s", got)
	}
}

func TestNewIDIsADestinationID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewID()
		if len(id) != 10 || id[0] != 'a' || seen[id] {
			t.Fatalf("%q", id)
		}
		seen[id] = true
	}
}
