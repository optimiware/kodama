package keys

// Published vectors for the SigV4 signer. Nothing expected here was produced
// by the signer under test.
//
// The request vectors are the AWS Signature Version 4 test suite
// ("aws-sig-v4-test-suite"), fetched from the awslabs/aws-c-auth mirror at
// commit bdb7cef99d8bd1ad3c189b1400140a7787e47105:
//
//	https://raw.githubusercontent.com/awslabs/aws-c-auth/main/tests/aws-signing-test-suite/v4/<name>/
//
// For each case the request comes from request.txt and context.json, the
// expected canonical request from header-canonical-request.txt, the string to
// sign from header-string-to-sign.txt, the signature from header-signature.txt
// and the Authorization value and any header the suite adds (X-Amz-Date,
// X-Amz-Security-Token, x-amz-content-sha256) from header-signed-request.txt.
// Every case uses the suite's fixed context: access key AKIDEXAMPLE, secret
// wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY, region us-east-1, service
// "service", time 2015-08-30T12:36:00Z.
//
// The signing-key derivation example is the one AWS published in "Examples of
// how to derive a signing key for Signature Version 4" (AWS General Reference,
// signature-v4-examples.html), read from the Internet Archive capture
// https://web.archive.org/web/20201205002306/https://docs.aws.amazon.com/general/latest/gr/signature-v4-examples.html

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	awsSigV4SuiteAccessKeyID = "AKIDEXAMPLE"
	awsSigV4SuiteSecretKey   = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	awsSigV4SuiteRegion      = "us-east-1"
	awsSigV4SuiteService     = "service"
)

// awsSigV4SuiteTime is the suite's fixed signing time.
func awsSigV4SuiteTime() time.Time {
	return time.Date(2015, time.August, 30, 12, 36, 0, 0, time.UTC)
}

// awsSigV4Vector is one published case. target is the request target exactly as
// it appears on the vector's request line; headers are the headers the
// published canonical request signs, with the values of the published signed
// request.
type awsSigV4Vector struct {
	name          string
	method        string
	target        string
	headers       map[string]string
	payload       string
	canonical     string
	stringToSign  string
	signature     string
	authorization string
}

var awsSigV4Vectors = []awsSigV4Vector{
	{
		name:   "get-vanilla",
		method: "GET",
		target: "/",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63`,
		signature:     "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
	},
	{
		name:   "post-vanilla",
		method: "POST",
		target: "/",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `POST
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
553f88c9e4d10fc9e109e2aeb65f030801b70c2f6468faca261d401ae622fc87`,
		signature:     "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
	},
	{
		name:   "post-vanilla-query",
		method: "POST",
		target: "/?Param1=value1",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `POST
/
Param1=value1
host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
9d659678c1756bb3113e2ce898845a0a79dbbc57b740555917687f1b3340fbbd`,
		signature:     "28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11",
	},
	{
		name:   "post-x-www-form-urlencoded",
		method: "POST",
		target: "/",
		headers: map[string]string{
			"Content-Type":         "application/x-www-form-urlencoded",
			"Host":                 "example.amazonaws.com",
			"Content-Length":       "13",
			"X-Amz-Date":           "20150830T123600Z",
			"x-amz-content-sha256": "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		},
		payload: "Param1=value1",
		canonical: `POST
/

content-length:13
content-type:application/x-www-form-urlencoded
host:example.amazonaws.com
x-amz-content-sha256:9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e
x-amz-date:20150830T123600Z

content-length;content-type;host;x-amz-content-sha256;x-amz-date
9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
b1edd1d03544c25390e32085d55b57acc9a3961bb59415ff86c45c3d89d16cfb`,
		signature:     "d3875051da38690788ef43de4db0d8f280229d82040bfac253562e56c3f20e0b",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-length;content-type;host;x-amz-content-sha256;x-amz-date, Signature=d3875051da38690788ef43de4db0d8f280229d82040bfac253562e56c3f20e0b",
	},
	{
		name:   "post-x-www-form-urlencoded-parameters",
		method: "POST",
		target: "/",
		headers: map[string]string{
			"Content-Type":         "application/x-www-form-urlencoded; charset=utf-8",
			"Host":                 "example.amazonaws.com",
			"Content-Length":       "13",
			"X-Amz-Date":           "20150830T123600Z",
			"x-amz-content-sha256": "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		},
		payload: "Param1=value1",
		canonical: `POST
/

content-length:13
content-type:application/x-www-form-urlencoded; charset=utf-8
host:example.amazonaws.com
x-amz-content-sha256:9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e
x-amz-date:20150830T123600Z

content-length;content-type;host;x-amz-content-sha256;x-amz-date
9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
a89f1a5b53e37702ee6363ce1da3ce8f54386f3c8f352ae652153c2982a0bc4d`,
		signature:     "328d1b9eaadca9f5818ef05e8392801e091653bafec24fcab71e7344e7f51422",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-length;content-type;host;x-amz-content-sha256;x-amz-date, Signature=328d1b9eaadca9f5818ef05e8392801e091653bafec24fcab71e7344e7f51422",
	},
	{
		name:   "post-sts-header-before",
		method: "POST",
		target: "/",
		headers: map[string]string{
			"Host":                 "example.amazonaws.com",
			"X-Amz-Security-Token": "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA==",
			"X-Amz-Date":           "20150830T123600Z",
		},
		canonical: `POST
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z
x-amz-security-token:AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA==

host;x-amz-date;x-amz-security-token
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
c237e1b440d4c63c32ca95b5b99481081cb7b13c7e40434868e71567c1a882f6`,
		signature:     "85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead",
	},
	{
		name:   "post-sts-header-after",
		method: "POST",
		target: "/",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `POST
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
553f88c9e4d10fc9e109e2aeb65f030801b70c2f6468faca261d401ae622fc87`,
		signature:     "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
	},
	{
		name:   "get-vanilla-with-session-token",
		method: "GET",
		target: "/",
		headers: map[string]string{
			"Host":                 "example.amazonaws.com",
			"X-Amz-Security-Token": "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267",
			"X-Amz-Date":           "20150830T123600Z",
		},
		canonical: `GET
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z
x-amz-security-token:6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267

host;x-amz-date;x-amz-security-token
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
067b36aa60031588cea4a4cde1f21215227a047690c72247f1d70b32fbbfad2b`,
		signature:     "07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8",
	},
	{
		name:   "get-vanilla-query-order-encoded",
		method: "GET",
		target: "/?Param-3=Value3&Param=Value2&%E1%88%B4=Value1",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/
%E1%88%B4=Value1&Param=Value2&Param-3=Value3
host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
868294f5c38bd141c4972a373a76654f1418a8e4fc18b2e7903ae45e8ae0ec71`,
		signature:     "371d3713e185cc334048618a97f809c9ffe339c62934c032af5a0e595648fcac",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=371d3713e185cc334048618a97f809c9ffe339c62934c032af5a0e595648fcac",
	},
	{
		name:   "get-vanilla-utf8-query",
		method: "GET",
		target: "/?ሴ=bar",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/
%E1%88%B4=bar
host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
eb30c5bed55734080471a834cc727ae56beb50e5f39d1bff6d0d38cb192a7073`,
		signature:     "2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04",
	},
	{
		name:   "get-unreserved",
		method: "GET",
		target: "/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
6a968768eefaa713e2a6b16b589a8ea192661f098f37349f4e2c0082757446f9`,
		signature:     "07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f",
	},
	{
		name:   "get-utf8",
		method: "GET",
		target: "/ሴ",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/%E1%88%B4

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
2a0a97d02205e45ce2e994789806b19270cfbbb0921b278ccf58f5249ac42102`,
		signature:     "8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85",
	},
	{
		name:   "get-space-normalized",
		method: "GET",
		target: "/example space/",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/example%20space/

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
63ee75631ed7234ae61b5f736dfc7754cdccfedbff4b5128a915706ee9390d86`,
		signature:     "652487583200325589f1fba4c7e578f72c47cb61beeca81406b39ddec1366741",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=652487583200325589f1fba4c7e578f72c47cb61beeca81406b39ddec1366741",
	},
	{
		name:   "get-header-value-trim",
		method: "GET",
		target: "/",
		headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"My-Header1": " value1",
			"My-Header2": " \"a   b   c\"",
			"X-Amz-Date": "20150830T123600Z",
		},
		canonical: `GET
/

host:example.amazonaws.com
my-header1:value1
my-header2:"a b c"
x-amz-date:20150830T123600Z

host;my-header1;my-header2;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		stringToSign: `AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
a726db9b0df21c14f559d0a978e563112acb1b9e05476f0a6a1c7d68f28605c7`,
		signature:     "acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736",
		authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;my-header1;my-header2;x-amz-date, Signature=acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736",
	},
}

// awsSigV4VectorRequest turns a published request into the signer's input.
func awsSigV4VectorRequest(t *testing.T, vector awsSigV4Vector) awsSigV4Request {
	t.Helper()
	path, rawQuery, _ := strings.Cut(vector.target, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("parse vector query %q: %v", rawQuery, err)
	}
	return awsSigV4Request{
		method:  vector.method,
		path:    path,
		query:   query,
		headers: vector.headers,
		payload: []byte(vector.payload),
	}
}

// TestAWSSigV4PublishedSuiteVectors pins the canonical request, the string to
// sign and the Authorization header of every embedded suite case. The cases
// cover what the KMS client sends (POST /, a signed session-token header, a
// signed content type and a body) and the encoding rules the signer implements
// beyond it: query ordering and encoding, unreserved and UTF-8 path bytes, a
// space in the path and header value trimming.
func TestAWSSigV4PublishedSuiteVectors(t *testing.T) {
	credential := awsSigV4Credential{
		accessKeyID:     awsSigV4SuiteAccessKeyID,
		secretAccessKey: awsSigV4SuiteSecretKey,
		region:          awsSigV4SuiteRegion,
		service:         awsSigV4SuiteService,
	}
	for _, vector := range awsSigV4Vectors {
		t.Run(vector.name, func(t *testing.T) {
			request := awsSigV4VectorRequest(t, vector)
			canonical, signedHeaders := awsSigV4CanonicalRequest(request)
			if canonical != vector.canonical {
				t.Fatalf("canonical request =\n%s\nwant the published\n%s", canonical, vector.canonical)
			}
			scope := "20150830/" + awsSigV4SuiteRegion + "/" + awsSigV4SuiteService + "/aws4_request"
			stringToSign := awsSigV4StringToSign(awsSigV4SuiteTime(), scope, canonical)
			if stringToSign != vector.stringToSign {
				t.Fatalf("string to sign =\n%s\nwant the published\n%s", stringToSign, vector.stringToSign)
			}
			authorization := awsSigV4Authorization(credential, awsSigV4SuiteTime(), request)
			if authorization != vector.authorization {
				t.Fatalf("Authorization =\n%s\nwant the published\n%s", authorization, vector.authorization)
			}
			// The fixture is self-consistent: the published Authorization value
			// carries the published signature and the signed-header list.
			want := "AWS4-HMAC-SHA256 Credential=" + awsSigV4SuiteAccessKeyID + "/" + scope +
				", SignedHeaders=" + signedHeaders + ", Signature=" + vector.signature
			if vector.authorization != want {
				t.Fatalf("fixture Authorization %q does not carry the published signature %q", vector.authorization, vector.signature)
			}
		})
	}
}

// TestAWSSigV4SessionTokenHeaderIsSigned pins which of the suite's two
// session-token cases the client follows. post-sts-header-before signs
// X-Amz-Security-Token; post-sts-header-after adds it after signing, which is
// the same signature as post-vanilla. The KMS client signs the token, so a
// signer that dropped the header would produce the other published signature.
func TestAWSSigV4SessionTokenHeaderIsSigned(t *testing.T) {
	byName := make(map[string]awsSigV4Vector, len(awsSigV4Vectors))
	for _, vector := range awsSigV4Vectors {
		byName[vector.name] = vector
	}
	before, after, vanilla := byName["post-sts-header-before"], byName["post-sts-header-after"], byName["post-vanilla"]
	if _, signed := before.headers["X-Amz-Security-Token"]; !signed {
		t.Fatal("post-sts-header-before does not sign X-Amz-Security-Token")
	}
	if _, signed := after.headers["X-Amz-Security-Token"]; signed {
		t.Fatal("post-sts-header-after signs X-Amz-Security-Token")
	}
	if after.signature != vanilla.signature {
		t.Fatal("post-sts-header-after is not the post-vanilla signature")
	}
	if before.signature == after.signature {
		t.Fatal("the signed and unsigned session-token cases share a signature")
	}
	if !strings.Contains(before.authorization, "SignedHeaders=host;x-amz-date;x-amz-security-token,") {
		t.Fatalf("post-sts-header-before signed headers = %q", before.authorization)
	}
}

// TestAWSSigV4SigningKeyDerivationExample pins the published key-derivation
// example: every intermediate key and the final signing key.
func TestAWSSigV4SigningKeyDerivationExample(t *testing.T) {
	const (
		secretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		date      = "20120215"
		region    = "us-east-1"
		service   = "iam"

		wantSecret  = "41575334774a616c725855746e46454d492f4b374d44454e472b62507852666943594558414d504c454b4559"
		wantDate    = "969fbb94feb542b71ede6f87fe4d5fa29c789342b0f407474670f0c2489e0a0d"
		wantRegion  = "69daa0209cd9c5ff5c8ced464a696fd4252e981430b10e3d3fd8e2f197d7a70c"
		wantService = "f72cfd46f26bc4643f06a11eabb6c0ba18780c19a8da0c31ace671265e3c87fa"
		wantSigning = "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"
	)
	if got := hex.EncodeToString([]byte("AWS4" + secretKey)); got != wantSecret {
		t.Fatalf("kSecret = %s; want %s", got, wantSecret)
	}
	dateKey := awsSigV4HMAC([]byte("AWS4"+secretKey), date)
	if got := hex.EncodeToString(dateKey); got != wantDate {
		t.Fatalf("kDate = %s; want %s", got, wantDate)
	}
	regionKey := awsSigV4HMAC(dateKey, region)
	if got := hex.EncodeToString(regionKey); got != wantRegion {
		t.Fatalf("kRegion = %s; want %s", got, wantRegion)
	}
	serviceKey := awsSigV4HMAC(regionKey, service)
	if got := hex.EncodeToString(serviceKey); got != wantService {
		t.Fatalf("kService = %s; want %s", got, wantService)
	}
	if got := hex.EncodeToString(awsSigV4HMAC(serviceKey, "aws4_request")); got != wantSigning {
		t.Fatalf("kSigning = %s; want %s", got, wantSigning)
	}
	if got := hex.EncodeToString(awsSigV4SigningKey(secretKey, date, region, service)); got != wantSigning {
		t.Fatalf("awsSigV4SigningKey = %s; want the published %s", got, wantSigning)
	}
}

// TestAWSSigV4Timestamp pins the X-Amz-Date rendering: UTC, basic ISO 8601,
// whatever zone the clock reports.
func TestAWSSigV4Timestamp(t *testing.T) {
	zone := time.FixedZone("plus-two", 2*60*60)
	at := time.Date(2015, time.August, 30, 14, 36, 0, 987, zone)
	if got := awsSigV4Timestamp(at); got != "20150830T123600Z" {
		t.Fatalf("timestamp = %q; want 20150830T123600Z", got)
	}
}
