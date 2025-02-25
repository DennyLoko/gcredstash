package internal

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/kms/kmsiface"
)

var (
	ErrItemNotFound   = errors.New("item couldn't be found")
	ErrNeedContext    = errors.New("could not decrypt HMAC key with KMS: the credential may require that an encryption context be provided to decrypt it")
	ErrCredNotMatched = errors.New("could not decrypt HMAC key with KMS: the encryption context provided may not match the one used when the credential was stored")
	ErrBadHMAC        = errors.New("computed HMAC does not match stored HMAC")
	ErrVersionExists  = errors.New("version already in the credential store - use the -v flag to specify a new version")
)

type Driver struct {
	Ddb dynamodbiface.DynamoDBAPI
	Kms kmsiface.KMSAPI
}

func NewDriver() (*Driver, error) {
	awsSession, err := session.NewSession()
	if err != nil {
		return nil, fmt.Errorf("cannot create session: %w", err)
	}
	driver := &Driver{
		Ddb: dynamodb.New(awsSession),
		Kms: kms.New(awsSession),
	}
	return driver, nil
}

func (driver *Driver) GetMaterialWithoutVersion(name, table string) (map[string]*dynamodb.AttributeValue, error) {
	params := &dynamodb.QueryInput{
		TableName:                aws.String(table),
		Limit:                    aws.Int64(1),
		ConsistentRead:           aws.Bool(true),
		ScanIndexForward:         aws.Bool(false),
		KeyConditionExpression:   aws.String("#name = :name"),
		ExpressionAttributeNames: map[string]*string{"#name": aws.String("name")},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":name": {S: aws.String(name)},
		},
	}

	resp, err := driver.Ddb.Query(params)
	if err != nil {
		//nolint:wrapcheck
		return nil, err
	}

	if *resp.Count == 0 {
		return nil, fmt.Errorf(`%w: {"name": %q}`, ErrItemNotFound, name)
	}

	return resp.Items[0], nil
}

func (driver *Driver) GetMaterialWithVersion(name, version, table string) (map[string]*dynamodb.AttributeValue, error) {
	params := &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]*dynamodb.AttributeValue{
			"name":    {S: aws.String(name)},
			"version": {S: aws.String(version)},
		},
	}

	resp, err := driver.Ddb.GetItem(params)
	if err != nil {
		//nolint:wrapcheck
		return nil, err
	}

	if resp.Item == nil {
		return nil, fmt.Errorf(`%w: {"name": %q}`, ErrItemNotFound, name)
	}

	return resp.Item, nil
}

func (driver *Driver) DecryptMaterial(name string, material map[string]*dynamodb.AttributeValue, context map[string]string) (string, error) {
	data := B64Decode(*material["key"].S)
	dataKey, hmacKey, err := KmsDecrypt(driver.Kms, data, context)
	if err != nil {
		if strings.Contains(err.Error(), "InvalidCiphertextException") {
			if len(context) < 1 {
				return "", fmt.Errorf("%s: %w", name, ErrNeedContext)
			}
			return "", fmt.Errorf("%s: %w", name, ErrCredNotMatched)
		}
		return "", err
	}

	var hmac []byte
	if len(material["hmac"].B) == 0 {
		hmac = HexDecode(*material["hmac"].S)
	} else {
		hmac = HexDecode(string(material["hmac"].B))
	}

	contents := B64Decode(*material["contents"].S)
	if !ValidateHMAC(contents, hmac, hmacKey) {
		return "", fmt.Errorf("%s: %w", name, ErrBadHMAC)
	}

	decrypted := Crypt(contents, dataKey)

	return string(decrypted), nil
}

func (driver *Driver) GetHighestVersion(name, table string) (int, error) {
	params := &dynamodb.QueryInput{
		TableName:                aws.String(table),
		Limit:                    aws.Int64(1),
		ConsistentRead:           aws.Bool(true),
		ScanIndexForward:         aws.Bool(false),
		KeyConditionExpression:   aws.String("#name = :name"),
		ExpressionAttributeNames: map[string]*string{"#name": aws.String("name")},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":name": {S: aws.String(name)},
		},
		ProjectionExpression: aws.String("version"),
	}

	resp, err := driver.Ddb.Query(params)
	if err != nil {
		return -1, fmt.Errorf("can't query version: %w", err)
	}

	if *resp.Count == 0 {
		return 0, nil
	}

	version := *resp.Items[0]["version"].S
	versionNum := Atoi(version)

	return versionNum, nil
}

func (driver *Driver) PutItem(name, version string, key, contents, hmac []byte, table string) error {
	b64key := B64Encode(key)
	b64contents := B64Encode(contents)
	hexHmac := HexEncode(hmac)

	params := &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item: map[string]*dynamodb.AttributeValue{
			"name":     {S: aws.String(name)},
			"version":  {S: aws.String(version)},
			"key":      {S: aws.String(b64key)},
			"contents": {S: aws.String(b64contents)},
			"hmac":     {S: aws.String(hexHmac)},
		},
		ConditionExpression:      aws.String("attribute_not_exists(#name)"),
		ExpressionAttributeNames: map[string]*string{"#name": aws.String("name")},
	}

	_, err := driver.Ddb.PutItem(params)
	if err != nil {
		return fmt.Errorf("can't store secret: %w", err)
	}

	return nil
}

func (driver *Driver) GetDeleteTargetWithoutVersion(name, table string) (map[*string]*string, error) {
	items := map[*string]*string{}

	params := &dynamodb.QueryInput{
		TableName:                aws.String(table),
		ConsistentRead:           aws.Bool(true),
		KeyConditionExpression:   aws.String("#name = :name"),
		ExpressionAttributeNames: map[string]*string{"#name": aws.String("name")},
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":name": {S: aws.String(name)},
		},
	}

	resp, err := driver.Ddb.Query(params)
	if err != nil {
		return nil, fmt.Errorf("can't find deletion target: %w", err)
	}

	if *resp.Count == 0 {
		return nil, fmt.Errorf(`%w: {"name": %q}`, ErrItemNotFound, name)
	}

	for _, i := range resp.Items {
		items[i["name"].S] = i["version"].S
	}

	return items, nil
}

func (driver *Driver) GetDeleteTargetWithVersion(name, version, table string) (map[*string]*string, error) {
	params := &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]*dynamodb.AttributeValue{
			"name":    {S: aws.String(name)},
			"version": {S: aws.String(version)},
		},
	}

	resp, err := driver.Ddb.GetItem(params)
	if err != nil {
		return nil, fmt.Errorf("can't find deletion target: %w", err)
	}

	if resp.Item == nil {
		versionNum := Atoi(version)
		return nil, fmt.Errorf(`%w: {"name": %q, "version": %d}`, ErrItemNotFound, name, versionNum)
	}

	items := map[*string]*string{}
	items[resp.Item["name"].S] = resp.Item["version"].S

	return items, nil
}

func (driver *Driver) DeleteItem(name, version, table string) error {
	svc := driver.Ddb

	params := &dynamodb.DeleteItemInput{
		TableName: aws.String(table),
		Key: map[string]*dynamodb.AttributeValue{
			"name":    {S: aws.String(name)},
			"version": {S: aws.String(version)},
		},
	}

	if _, err := svc.DeleteItem(params); err != nil {
		return fmt.Errorf("can't delete secret %q (%v): %w", name, version, err)
	}

	return nil
}

func (driver *Driver) DeleteSecrets(name, version, table string) error {
	var items map[*string]*string
	var err error

	if version == "" {
		items, err = driver.GetDeleteTargetWithoutVersion(name, table)
	} else {
		items, err = driver.GetDeleteTargetWithVersion(name, version, table)
	}

	if err != nil {
		return err
	}

	for name, version := range items {
		err := driver.DeleteItem(*name, *version, table)
		if err != nil {
			return err
		}

		versionNum := Atoi(*version)
		fmt.Fprintf(os.Stderr, "Deleting %s -- version %d\n", *name, versionNum)
	}

	return nil
}

func (driver *Driver) PutSecret(name, secret, version, kmsKey, table string, context map[string]string) error {
	dataKey, hmacKey, wrappedKey, err := KmsGenerateDataKey(driver.Kms, kmsKey, context)
	if err != nil {
		return fmt.Errorf("could not generate key using KMS key(%s): %w", kmsKey, err)
	}

	cipherText := Crypt([]byte(secret), dataKey)
	hmac := Digest(cipherText, hmacKey)

	err = driver.PutItem(name, version, wrappedKey, cipherText, hmac, table)

	if err != nil {
		if strings.Contains(err.Error(), "ConditionalCheckFailedException") {
			latestVersion, err := driver.GetHighestVersion(name, table)
			if err != nil {
				//nolint:wrapcheck
				return err
			}

			return fmt.Errorf("%w (name: %q, version: %d)", ErrVersionExists, name, latestVersion)
		}
		return err
	}

	return nil
}

func (driver *Driver) GetSecret(name, version, table string, context map[string]string) (string, error) {
	var material map[string]*dynamodb.AttributeValue
	var err error

	if version == "" {
		material, err = driver.GetMaterialWithoutVersion(name, table)
	} else {
		material, err = driver.GetMaterialWithVersion(name, version, table)
	}

	if err != nil {
		return "", fmt.Errorf("can't fetch secret: %w", err)
	}

	value, err := driver.DecryptMaterial(name, material, context)
	if err != nil {
		return "", fmt.Errorf("can't decrypt secret: %w", err)
	}

	return value, nil
}

func (driver *Driver) ListSecrets(table string) (map[*string]*string, error) {
	svc := driver.Ddb

	params := &dynamodb.ScanInput{
		TableName:                aws.String(table),
		ProjectionExpression:     aws.String("#name,version"),
		ExpressionAttributeNames: map[string]*string{"#name": aws.String("name")},
	}

	resp, err := svc.Scan(params)
	if err != nil {
		return nil, fmt.Errorf("can't list secrets: %w", err)
	}

	items := map[*string]*string{}

	for _, i := range resp.Items {
		items[i["name"].S] = i["version"].S
	}

	return items, nil
}
