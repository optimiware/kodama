// Command purge-session-objects deletes the objects a qualification session left under
// qualification/<session>/ in the artifacts bucket, every version and delete marker included. It refuses any other prefix, so it can never touch the
// installation's own data (kodama/), the Loki bucket or another session's prefix by accident.
//
// Credentials come from the environment (PURGE_ACCESS_KEY, PURGE_SECRET_KEY) and are never printed.
//
//	purge-session-objects --endpoint https://s3.fr-par.scw.cloud --region fr-par --bucket b --session kg
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var sessionName = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)

// purgeCurrent is the fallback for a credential that may not list object versions (the stage runtime key): it
// deletes the current objects, which in a versioned bucket only adds delete markers. It says so, because every
// earlier version of those objects remains until a credential or a lifecycle rule removes them.
func purgeCurrent(client *s3.Client, bucket *string, prefix string) {
	deleted := 0
	for {
		page, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String(prefix), MaxKeys: aws.Int32(500)})
		if err != nil {
			fmt.Fprintf(os.Stderr, "purge-session-objects: list failed (%s)\n", failure(err))
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
	fmt.Printf("WARNING: this credential cannot list object versions: %d current objects under %s were deleted (delete markers added); every earlier version remains\n", deleted, prefix)
}

// failure names an error by its provider code and type only, never its message.
func failure(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return fmt.Sprintf("%T", err)
}

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
	// The artifacts bucket is versioned: deleting a key adds a delete marker and keeps every version. Purge
	// removes each version and each delete marker under the prefix by version id, so nothing of the session stays.
	deleted := 0
	var keyMarker, versionMarker *string
	for {
		page, err := client.ListObjectVersions(context.Background(), &s3.ListObjectVersionsInput{
			Bucket: bucket, Prefix: aws.String(prefix), MaxKeys: aws.Int32(500), KeyMarker: keyMarker, VersionIdMarker: versionMarker,
		})
		if err != nil {
			if failure(err) == "AccessDenied" && deleted == 0 {
				purgeCurrent(client, bucket, prefix)
				return
			}
			fmt.Fprintf(os.Stderr, "purge-session-objects: list failed (%s)\n", failure(err))
			os.Exit(1)
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Versions)+len(page.DeleteMarkers))
		for _, v := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) > 0 {
			out, err := client.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
			if err != nil || len(out.Errors) > 0 {
				fmt.Fprintln(os.Stderr, "purge-session-objects: delete failed")
				os.Exit(1)
			}
			deleted += len(ids)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	fmt.Printf("purged %d object versions and delete markers under %s\n", deleted, prefix)
}
