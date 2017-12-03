package aws

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/convox/rack/api/crypt"
	"github.com/convox/rack/structs"
	"github.com/convox/rack/manifest"
)

// ReleaseDelete will delete all releases that belong to app and buildID
// This could includes the active release which implies this should be called with caution.
func (p *AWSProvider) ReleaseDelete(app, buildID string) error {
	qi := &dynamodb.QueryInput{
		KeyConditionExpression: aws.String("app = :app"),
		FilterExpression:       aws.String("build = :build"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":app":   {S: aws.String(app)},
			":build": {S: aws.String(buildID)},
		},
		IndexName: aws.String("app.created"),
		TableName: aws.String(p.DynamoReleases),
	}

	return p.deleteReleaseItems(qi, p.DynamoReleases)
}

// ReleaseGet returns a release
func (p *AWSProvider) ReleaseGet(app, id string) (*structs.Release, error) {
	if id == "" {
		return nil, fmt.Errorf("release id must not be empty")
	}

	item, err := p.fetchRelease(app, id)
	if err != nil {
		return nil, err
	}

	r, err := releaseFromItem(item)
	if err != nil {
		return nil, err
	}

	settings, err := p.appResource(app, "Settings")
	if err != nil {
		return nil, err
	}

	data, err := p.s3Get(settings, fmt.Sprintf("releases/%s/env", r.Id))
	if err != nil {
		return nil, err
	}

	key, err := p.rackResource("EncryptionKey")
	if err != nil {
		return nil, err
	}

	if key != "" {
		if d, err := crypt.New().Decrypt(key, data); err == nil {
			data = d
		}
	}

	env := structs.Environment{}

	if err := env.Load(data); err != nil {
		return nil, err
	}

	r.Env = env.String()

	return r, nil
}

// ReleaseList returns a list of the latest releases, with the length specified in limit
func (p *AWSProvider) ReleaseList(app string, limit int64) (structs.Releases, error) {
	a, err := p.AppGet(app)
	if err != nil {
		return nil, err
	}

	req := &dynamodb.QueryInput{
		KeyConditions: map[string]*dynamodb.Condition{
			"app": {
				AttributeValueList: []*dynamodb.AttributeValue{
					{S: aws.String(a.Name)},
				},
				ComparisonOperator: aws.String("EQ"),
			},
		},
		IndexName:        aws.String("app.created"),
		Limit:            aws.Int64(limit),
		ScanIndexForward: aws.Bool(false),
		TableName:        aws.String(p.DynamoReleases),
	}

	res, err := p.dynamodb().Query(req)
	if err != nil {
		return nil, err
	}

	releases := make(structs.Releases, len(res.Items))

	for i, item := range res.Items {
		r, err := releaseFromItem(item)
		if err != nil {
			return nil, err
		}

		releases[i] = *r
	}

	return releases, nil
}

// ReleasePromote promotes a release
func (p *AWSProvider) ReleasePromote(r *structs.Release) error {
	if _, err := p.AppGet(r.App); err != nil {
		return err
	}

	env := structs.Environment{}

	if err := env.Load([]byte(r.Env)); err != nil {
		return err
	}

	m, err := manifest.Load([]byte(r.Manifest), manifest.Environment(env))
	if err != nil {
		return err
	}

	for _, s := range m.Services {
		if s.Internal && !p.Internal {
			return fmt.Errorf("rack does not support internal services")
		}
	}

	tp := map[string]interface{}{
		"App":      r.App,
		"Env":      env,
		"Manifest": m,
		"Release":  r,
		"Version":  p.Release,
	}

	data, err := formationTemplate("app", tp)
	if err != nil {
		return err
	}

	// fmt.Printf("string(data) = %+v\n", string(data))

	ou, err := p.ObjectStore("", bytes.NewReader(data), structs.ObjectOptions{})
	if err != nil {
		return err
	}

	updates := map[string]string{
		"LogBucket": p.LogBucket,
	}

	if err := p.updateStack(p.rackStack(r.App), ou, updates); err != nil {
		return err
	}

	go p.waitForPromotion(r)

	return nil
}

// ReleaseSave saves a Release
func (p *AWSProvider) ReleaseSave(r *structs.Release) error {
	if r.Id == "" {
		return fmt.Errorf("Id can not be blank")
	}

	if r.Created.IsZero() {
		r.Created = time.Now()
	}

	if p.IsTest() {
		r.Created = time.Unix(1473028693, 0).UTC()
	}

	req := &dynamodb.PutItemInput{
		Item: map[string]*dynamodb.AttributeValue{
			"id":      {S: aws.String(r.Id)},
			"app":     {S: aws.String(r.App)},
			"created": {S: aws.String(r.Created.Format(sortableTime))},
		},
		TableName: aws.String(p.DynamoReleases),
	}

	if r.Build != "" {
		req.Item["build"] = &dynamodb.AttributeValue{S: aws.String(r.Build)}
	}

	if r.Manifest != "" {
		req.Item["manifest"] = &dynamodb.AttributeValue{S: aws.String(r.Manifest)}
	}

	env := []byte(r.Env)

	key, err := p.rackResource("EncryptionKey")
	if err != nil {
		return err
	}

	if key != "" {
		env, err = crypt.New().Encrypt(key, []byte(env))
		if err != nil {
			return err
		}
	}

	settings, err := p.appResource(r.App, "Settings")
	if err != nil {
		return err
	}

	a, err := p.AppGet(r.App)
	if err != nil {
		return err
	}

	sreq := &s3.PutObjectInput{
		Body:          bytes.NewReader(env),
		Bucket:        aws.String(settings),
		ContentLength: aws.Int64(int64(len(env))),
		Key:           aws.String(fmt.Sprintf("releases/%s/env", r.Id)),
	}

	switch a.Tags["Generation"] {
	case "2":
	default:
		sreq.ACL = aws.String("public-read")
	}

	_, err = p.s3().PutObject(sreq)
	if err != nil {
		return err
	}

	_, err = p.dynamodb().PutItem(req)
	return err
}

func (p *AWSProvider) fetchRelease(app, id string) (map[string]*dynamodb.AttributeValue, error) {
	res, err := p.dynamodb().GetItem(&dynamodb.GetItemInput{
		ConsistentRead: aws.Bool(true),
		Key: map[string]*dynamodb.AttributeValue{
			"id": {S: aws.String(id)},
		},
		TableName: aws.String(p.DynamoReleases),
	})
	if err != nil {
		return nil, err
	}
	if res.Item == nil {
		return nil, errorNotFound(fmt.Sprintf("no such release: %s", id))
	}
	if res.Item["app"] == nil || *res.Item["app"].S != app {
		return nil, fmt.Errorf("mismatched app and release")
	}

	return res.Item, nil
}

func releaseFromItem(item map[string]*dynamodb.AttributeValue) (*structs.Release, error) {
	created, err := time.Parse(sortableTime, coalesce(item["created"], ""))
	if err != nil {
		return nil, err
	}

	release := &structs.Release{
		Id:       coalesce(item["id"], ""),
		App:      coalesce(item["app"], ""),
		Build:    coalesce(item["build"], ""),
		Manifest: coalesce(item["manifest"], ""),
		Created:  created,
	}

	return release, nil
}

// releasesDeleteAll will delete all releases associate with app
// This includes the active release which implies this should only be called when deleting an app.
func (p *AWSProvider) releaseDeleteAll(app string) error {

	// query dynamo for all releases for this app
	qi := &dynamodb.QueryInput{
		KeyConditionExpression: aws.String("app = :app"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":app": {S: aws.String(app)},
		},
		IndexName: aws.String("app.created"),
		TableName: aws.String(p.DynamoReleases),
	}

	return p.deleteReleaseItems(qi, p.DynamoReleases)
}

// deleteReleaseItems deletes release items from Dynamodb based on query input and the tableName
func (p *AWSProvider) deleteReleaseItems(qi *dynamodb.QueryInput, tableName string) error {
	res, err := p.dynamodb().Query(qi)
	if err != nil {
		return err
	}

	// collect release IDs to delete
	wrs := []*dynamodb.WriteRequest{}
	for _, item := range res.Items {
		r, err := releaseFromItem(item)
		if err != nil {
			return err
		}

		wr := &dynamodb.WriteRequest{
			DeleteRequest: &dynamodb.DeleteRequest{
				Key: map[string]*dynamodb.AttributeValue{
					"id": {
						S: aws.String(r.Id),
					},
				},
			},
		}

		wrs = append(wrs, wr)
	}

	return p.dynamoBatchDeleteItems(wrs, tableName)
}

func (p *AWSProvider) waitForPromotion(r *structs.Release) {
	event := &structs.Event{
		Action: "release:promote",
		Data: map[string]string{
			"app": r.App,
			"id":  r.Id,
		},
	}
	stackName := fmt.Sprintf("%s-%s", os.Getenv("RACK"), r.App)

	waitch := make(chan error)
	go func() {
		var err error
		//we have observed stack stabalization failures take up to 3 hours
		for i := 0; i < 3; i++ {
			err = p.cloudformation().WaitUntilStackUpdateComplete(&cloudformation.DescribeStacksInput{
				StackName: aws.String(stackName),
			})
			if err != nil {
				if err.Error() == "exceeded 120 wait attempts" {
					continue
				}
			}
			break
		}
		waitch <- err
	}()

	for {
		select {
		case err := <-waitch:
			if err == nil {
				event.Status = "success"
				p.EventSend(event, nil)
				return
			}

			if err != nil && err.Error() == "exceeded 120 wait attempts" {
				p.EventSend(event, fmt.Errorf("couldn't determine promotion status, timed out"))
				fmt.Println(fmt.Errorf("couldn't determine promotion status, timed out"))
				return
			}

			resp, err := p.cloudformation().DescribeStacks(&cloudformation.DescribeStacksInput{
				StackName: aws.String(stackName),
			})
			if err != nil {
				p.EventSend(event, fmt.Errorf("unable to check stack status: %s", err))
				fmt.Println(fmt.Errorf("unable to check stack status: %s", err))
				return
			}

			if len(resp.Stacks) < 1 {
				p.EventSend(event, fmt.Errorf("app stack was not found: %s", stackName))
				fmt.Println(fmt.Errorf("app stack was not found: %s", stackName))
				return
			}

			se, err := p.cloudformation().DescribeStackEvents(&cloudformation.DescribeStackEventsInput{
				StackName: aws.String(stackName),
			})
			if err != nil {
				p.EventSend(event, fmt.Errorf("unable to check stack events: %s", err))
				fmt.Println(fmt.Errorf("unable to check stack events: %s", err))
				return
			}

			var lastEvent *cloudformation.StackEvent

			for _, e := range se.StackEvents {
				switch *e.ResourceStatus {
				case "UPDATE_FAILED", "DELETE_FAILED", "CREATE_FAILED":
					lastEvent = e
					break
				}
			}

			ee := fmt.Errorf("unable to determine release error")
			if lastEvent != nil {
				ee = fmt.Errorf(
					"[%s:%s] [%s]: %s",
					*lastEvent.ResourceType,
					*lastEvent.LogicalResourceId,
					*lastEvent.ResourceStatus,
					*lastEvent.ResourceStatusReason,
				)
			}

			p.EventSend(event, fmt.Errorf("release %s failed - %s", r.Id, ee.Error()))
		}
	}
}
