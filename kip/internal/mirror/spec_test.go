package mirror

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseURL(t *testing.T) {
	endpoint, bucket, err := ParseURL("s3://fsn1.objects.example.com/app-uploads")
	require.NoError(t, err)
	assert.Equal(t, "fsn1.objects.example.com", endpoint)
	assert.Equal(t, "app-uploads", bucket)

	endpoint, bucket, err = ParseURL("s3://Objects.Example.com:9000/bk1/")
	require.NoError(t, err)
	assert.Equal(t, "objects.example.com:9000", endpoint, "the host is case-insensitive and keeps its port")
	assert.Equal(t, "bk1", bucket)

	endpoint, _, err = ParseURL("s3://objects.example.com:443/bk1")
	require.NoError(t, err)
	assert.Equal(t, "objects.example.com", endpoint, "the HTTPS default port names the same endpoint")
}

func TestParseURLRejectsWhatItCannotMirror(t *testing.T) {
	for _, raw := range []string{
		"",
		"https://objects.example.com/bucket",
		"s3://objects.example.com",
		"s3://objects.example.com/",
		"s3:///bucket",
		"s3://objects.example.com/bucket/prefix",
		"s3://user:pass@objects.example.com/bucket",
		"s3://objects.example.com/bucket?x=1",
		"s3://objects.example.com/Bad_Bucket",
	} {
		_, _, err := ParseURL(raw)
		assert.Error(t, err, raw)
	}
}

func TestValidateName(t *testing.T) {
	assert.NoError(t, ValidateName("uploads"))
	assert.NoError(t, ValidateName(strings.Repeat("a", 38)))
	assert.Error(t, ValidateName(strings.Repeat("a", 39)), "backup-mirror-<name> must fit the 52-character CronJob limit")
	assert.Error(t, ValidateName("Uploads"))
	assert.Error(t, ValidateName(""))
	assert.Error(t, ValidateName("-x"))
}

func validSpec() Spec {
	return Spec{
		Name:          "uploads",
		Source:        Remote{Endpoint: "src.example.com", Bucket: "app", Region: "r1", AccessKeyID: "AK1", SecretAccessKey: "SK1"},
		Target:        Remote{Endpoint: "dst.example.com", Bucket: "app-backup", Region: "r2", AccessKeyID: "AK2", SecretAccessKey: "SK2"},
		RetentionDays: 30,
		MaxDelete:     -1,
	}
}

func TestSpecValidate(t *testing.T) {
	require.NoError(t, validSpec().Validate())

	cases := map[string]func(*Spec){
		"no retention":     func(s *Spec) { s.RetentionDays = 0 },
		"no source region": func(s *Spec) { s.Source.Region = "" },
		"no target key":    func(s *Spec) { s.Target.AccessKeyID = "" },
		"no target secret": func(s *Spec) { s.Target.SecretAccessKey = "" },
		"no source bucket": func(s *Spec) { s.Source.Bucket = "" },
		"bad name":         func(s *Spec) { s.Name = "Bad" },
	}
	for name, mutate := range cases {
		s := validSpec()
		mutate(&s)
		assert.Error(t, s.Validate(), name)
	}
}

func TestScheduleIntervalIsTheLongestGap(t *testing.T) {
	daily, err := ScheduleInterval("0 3 * * *")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, daily)

	sixHourly, err := ScheduleInterval("15 */6 * * *")
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, sixHourly)

	weekdays, err := ScheduleInterval("0 3 * * 1-5")
	require.NoError(t, err)
	assert.Equal(t, 72*time.Hour, weekdays, "Friday to Monday is the gap the alert has to tolerate")

	descriptor, err := ScheduleInterval("@daily")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, descriptor)
}

func TestScheduleIntervalRejectsInvalidSchedules(t *testing.T) {
	for _, s := range []string{"", "every day", "0 3 * *", "CRON_TZ=Europe/Berlin 0 3 * * *", "TZ=UTC 0 3 * * *"} {
		_, err := ScheduleInterval(s)
		assert.Error(t, err, s)
	}
}
