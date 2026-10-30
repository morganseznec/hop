package inventory

import (
	"testing"
	"time"
)

func TestDeriveEnv(t *testing.T) {
	cases := []struct {
		tags          map[string]string
		name, profile string
		want          string
	}{
		{nil, "acme-p-tools", "acme-shop", "prod"},
		{nil, "ec2-ew1-a-p-shop-app", "acme-shop", "prod"},
		{nil, "acme-t-app-v2", "acme-shop", "test"},
		{nil, "acme-shop-app", "acme-shop-retail-test", "test"},
		{map[string]string{"Environment": "Production"}, "acme-t-x", "", "prod"},
		{nil, "bastion", "acme-shop", ""},
	}
	for _, c := range cases {
		if got := DeriveEnv(c.tags, c.name, c.profile); got != c.want {
			t.Errorf("%s/%s: %q, want %q", c.name, c.profile, got, c.want)
		}
	}
}

func TestPutKeepsDataOnError(t *testing.T) {
	inv := &Inventory{Targets: map[string]*Target{}}
	ok := time.Now().Add(-time.Hour)
	inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: ok,
		Transcript: Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions"},
		Instances:  []Instance{{ID: "i-1", Name: "tools"}}})
	inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", Err: "expired", ErrKind: ErrSSOExpired, ErrAt: time.Now()})
	got := inv.Targets[TargetKey("acme-prod", "eu-west-1")]
	if len(got.Instances) != 1 || !got.FetchedAt.Equal(ok) || !got.Transcript.Enabled {
		t.Errorf("a failure wiped the previous data: %+v", got)
	}
	if got.Fresh(15*time.Minute, time.Now()) != true {
		t.Error("a pair in error must wait one minute before being retried")
	}
	if got.Fresh(15*time.Minute, time.Now().Add(2*time.Minute)) {
		t.Error("a pair in error must be retried after one minute")
	}
}

func TestInstancesDedup(t *testing.T) {
	inv := &Inventory{Targets: map[string]*Target{
		TargetKey("acme-shop", "eu-west-1"): {Profile: "acme-shop", Region: "eu-west-1", Instances: []Instance{
			{ID: "i-2", Name: "b", Profile: "acme-shop", Region: "eu-west-1"},
			{ID: "i-1", Name: "a", Profile: "acme-shop", Region: "eu-west-1"},
		}},
		TargetKey("acme-shop-readonly", "eu-west-1"): {Profile: "acme-shop-readonly", Region: "eu-west-1", Instances: []Instance{
			{ID: "i-1", Name: "a", Profile: "acme-shop-readonly", Region: "eu-west-1"},
		}},
		TargetKey("acme-prod", "eu-west-1"): {Profile: "acme-prod", Region: "eu-west-1", Instances: []Instance{
			{ID: "i-9", Name: "z", Profile: "acme-prod", Region: "eu-west-1"},
		}},
	}}
	got := inv.Instances([]string{
		TargetKey("acme-prod", "eu-west-1"),
		TargetKey("acme-shop", "eu-west-1"),
		TargetKey("acme-shop-readonly", "eu-west-1"),
	})
	if len(got) != 3 {
		t.Fatalf("%d instances: %+v", len(got), got)
	}
	if got[0].ID != "i-9" || got[1].ID != "i-1" || got[2].ID != "i-2" {
		t.Errorf("order: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	if len(got[1].AlsoVia) != 1 || got[1].AlsoVia[0] != "acme-shop-readonly" {
		t.Errorf("also_via: %v", got[1].AlsoVia)
	}
}

func TestParseSessionPrefs(t *testing.T) {
	on := ParseSessionPrefs(`{"schemaVersion":"1.0","sessionType":"Standard_Stream","inputs":{"s3BucketName":"","cloudWatchLogGroupName":"/acme/ssm/sessions","cloudWatchEncryptionEnabled":true,"kmsKeyId":"alias/x"}}`)
	if !on.Enabled || !on.Encrypted || on.Destination() != "/acme/ssm/sessions" {
		t.Errorf("enabled transcript misread: %+v", on)
	}
	off := ParseSessionPrefs(`{"inputs":{"s3BucketName":"","cloudWatchLogGroupName":""}}`)
	if off.Enabled || !off.Checked {
		t.Errorf("disabled transcript misread: %+v", off)
	}
}
