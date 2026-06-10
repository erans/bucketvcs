package buildtrigger

import (
	"reflect"
	"testing"
)

func TestSortedConnectorNames(t *testing.T) {
	aws := map[string]AWSConnector{"prod": {}, "dev": {}}
	azure := map[string]AzureConnector{"main": {}}
	awsNames, azureNames := SortedConnectorNames(aws, azure)
	if !reflect.DeepEqual(awsNames, []string{"dev", "prod"}) {
		t.Errorf("aws = %v", awsNames)
	}
	if !reflect.DeepEqual(azureNames, []string{"main"}) {
		t.Errorf("azure = %v", azureNames)
	}
	a, z := SortedConnectorNames(nil, nil)
	if a == nil || z == nil || len(a) != 0 || len(z) != 0 {
		t.Errorf("nil maps should give empty non-nil slices: %v %v", a, z)
	}
}
