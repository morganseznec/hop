package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// Name tags come from anyone allowed to tag an instance: scan prints them cleaned, like
// the inventory, and falls back to the ID when nothing printable is left.
func TestInstanceNamesInCleansTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationSet><item><instancesSet>`+
			`<item><instanceId>i-0e01</instanceId><tagSet><item><key>Name</key><value>&#x202E;acme&#x85;-p-tools</value></item></tagSet></item>`+
			`<item><instanceId>i-0e02</instanceId><tagSet><item><key>Name</key><value>&#x85;&#x202E;</value></item></tagSet></item>`+
			`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
	}))
	defer srv.Close()
	cfg := aws.Config{
		Region:       "eu-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		BaseEndpoint: aws.String(srv.URL),
	}
	names, err := instanceNamesIn(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "acme-p-tools,i-0e02" {
		t.Errorf("names = %q", got)
	}
}
