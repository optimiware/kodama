// Command purge-session-objects deletes the objects a qualification session left under
// qualification/<session>/ in the artifacts bucket. It refuses any other prefix, so it can never touch the
// installation's own data (kodama/), the Loki bucket or another session's prefix by accident.
//
// Credentials come from the environment (PURGE_ACCESS_KEY, PURGE_SECRET_KEY) and are never printed.
//
//	purge-session-objects --endpoint https://s3.fr-par.scw.cloud --region fr-par --bucket b --session kg
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var sessionName = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)

func main() {
	endpoint := flag.String("endpoint", "", "S3 endpoint (https)")
	region := flag.String("region", "", "S3 region")
	bucket := flag.String("bucket", "", "artifacts bucket")
	session := flag.String("session", "", "qualification session name")
	flag.Parse()
	if !sessionName.MatchString(*session) || *bucket == "" || *region == "" || len(*endpoint) < 9 || (*endpoint)[:8] != "https://" {
		fmt.Fprintln(os.Stderr, "purge-session-objects: a session, bucket, region and https endpoint are required")
		os.Exit(2)
	}
	prefix := "qualification/" + *session + "/"
	client := s3.New(s3.Options{
		Region: *region, BaseEndpoint: aws.String(*endpoint),
		Credentials: credentials.NewStaticCredentialsProvider(os.Getenv("PURGE_ACCESS_KEY"), os.Getenv("PURGE_SECRET_KEY"), ""),
	})
	deleted := 0
	for {
		page, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String(prefix), MaxKeys: aws.Int32(500)})
		if err != nil {
			fmt.Fprintf(os.Stderr, "purge-session-objects: list failed (%T)\n", err)
			os.Exit(1)
		}
		if len(page.Contents) == 0 {
			break
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, o := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		out, err := client.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
		if err != nil || len(out.Errors) > 0 {
			fmt.Fprintln(os.Stderr, "purge-session-objects: delete failed")
			os.Exit(1)
		}
		deleted += len(ids)
	}
	fmt.Printf("purged %d objects under %s\n", deleted, prefix)
}
