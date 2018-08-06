package aws

import (
        "fmt"
        "strings"
        "time"
        "github.com/aws/aws-sdk-go/aws/credentials"
)

const (
        Host string = "sts.amazonaws.com"

        // The AWS host to which this request is sent (sts.amazonaws.com)
        // is global and thus belongs to no single region; however, per
        // the AWS Signature V4 algorithm, the region must be specified
        // in the "string to sign". Therefore, "us-east-1" is used as an 
        // arbitrary value. The AWS administrator should enable STS in the 
        // region (i.e. in this case, enable STS in us-east-1)
        Region string = "us-east-1"

        Service string = "sts"

        RequestMethod string = "POST"

        RequestBody string = "Action=GetCallerIdentity&Version=2011-06-15"
)

type IAMAuthElements struct {
        Method  string
        URL     string
        Body    string
        Headers map[string]string
}


func GetIAMAuthElements() (*IAMAuthElements, error) {
        creds := &credentials.SharedCredentialsProvider{}
        val, err := creds.Retrieve()
        if err != nil {
                return nil, err
        }
        headers := makeSTSGetCallerIdentityRequestHeaders()
        url := fmt.Sprintf("https://%s/", Host)
        params := &RequestParams{
                Service:     Service,
                Region:      Region,
                Method:      RequestMethod,
                URL:         url,
                Headers:     headers,
                Body:        []byte(RequestBody),
                AccessKeyID: val.AccessKeyID,
                SecretKey:   val.SecretAccessKey,
        }
        auth, err := makeAuthorizationHeader(params)
        if err != nil {
                return nil, err
        }
        headers["Authorization"] = auth
        return &IAMAuthElements{
                Method:  RequestMethod,
                URL:     url,
                Body:    RequestBody,
                Headers: headers,
        }, nil
}

func makeSTSGetCallerIdentityRequestHeaders() map[string]string {
        now := nowAsISO8601()
        h := map[string]string{
                "Accept-Encoding": "identity",
                "Content-Type":    "application/x-www-form-urlencoded",
                "Host":            Host,
                "X-Amz-Date":      now,
        }
        h["Content-Length"] = fmt.Sprintf("%d", len(RequestBody))
        return h
}

func nowAsISO8601() string {
	return strings.Replace(strings.Replace(time.Now().UTC().Format(time.RFC3339)[:19] + "Z", "-", "", -1), ":", "", -1)
}