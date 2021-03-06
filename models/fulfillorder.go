package models

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"strconv"

	"github.com/Microsoft/ApplicationInsights-Go/appinsights"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"gopkg.in/matryer/try.v1"
)

var (
	database string
	password string
	status   string
)

var db string

var insightskey = "23c6b1ec-ca92-4083-86b6-eba851af9032"
var mongoURL = os.Getenv("MONGOURL")
var teamname = os.Getenv("TEAMNAME")
var isCosmosDb = strings.Contains(mongoURL, "documents.azure.com")

// MongoDB database and collection names
var mongoDatabaseName = "k8orders"
var mongoCollectionName = "orders"
var mongoDBSession *mgo.Session
var mongoDBSessionError error
var mongoPoolLimit = 25


var challengeTelemetryClient appinsights.TelemetryClient

// Order represents the order json
type Order struct {
	OrderID           string  `required:"false" description:"CosmoDB ID - will be autogenerated"`
	EmailAddress      string  `required:"true" description:"Email address of the customer"`
	PreferredLanguage string  `required:"false" description:"Preferred Language of the customer"`
	Product           string  `required:"false" description:"Product ordered by the customer"`
	Partition         string  `required:"false" description:"MongoDB Partition. Generated."`
	Total             float64 `required:"false" description:"Order total"`
	Source            string  `required:"false" description:"Source channel e.g. App Service, Container instance, K8 cluster etc"`
	Status            string  `required:"true" description:"Order Status"`
}

func init() {

	// Init App Insights
	challengeTelemetryClient = appinsights.NewTelemetryClient(insightskey)

	// Let's validate and spool the ENV VARS

	if len(os.Getenv("MONGOURL")) == 0 {
		log.Print("The environment variable MONGOURL has not been set")
	} else {
		log.Print("The environment variable MONGOURL is " + os.Getenv("MONGOURL"))
	}

	if len(os.Getenv("TEAMNAME")) == 0 {
		log.Print("The environment variable TEAMNAME has not been set")
	} else {
		log.Print("The environment variable TEAMNAME is " + os.Getenv("TEAMNAME"))
	}

	var mongoPoolLimitEnv = os.Getenv("MONGOPOOL_LIMIT")
	if mongoPoolLimitEnv != "" {
		if limit, err := strconv.Atoi(mongoPoolLimitEnv); err == nil {
			mongoPoolLimit = limit
		}
	}
	log.Printf("MongoDB pool limit set to %v. You can override by setting the MONGOPOOL_LIMIT environment variable." , mongoPoolLimit)
	


	url, err := url.Parse(mongoURL)
	if err != nil {
		log.Fatal(fmt.Sprintf("Problem parsing Mongo URL %s: ",url), err)
	}

	if isCosmosDb {
		log.Println("Using CosmosDB")
		db = "CosmosDB"

	} else {
		log.Println("Using MongoDB")
		db = "MongoDB"
	}

	// Parse the connection string to extract components because the MongoDB driver is peculiar
	var dialInfo *mgo.DialInfo
	mongoUsername := ""
	mongoPassword := ""
	if url.User!=nil {
		mongoUsername = url.User.Username()
		mongoPassword, _ = url.User.Password()
	}
	mongoHost := url.Host
	mongoDatabase := mongoDatabaseName // can be anything
	mongoSSL := strings.Contains(url.RawQuery, "ssl=true")

	log.Printf("\tUsername: %s", mongoUsername)
	log.Printf("\tPassword: %s", mongoPassword)
	log.Printf("\tHost: %s", mongoHost)
	log.Printf("\tDatabase: %s", mongoDatabase)
	log.Printf("\tSSL: %t", mongoSSL)

	if mongoSSL {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  60 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
			DialServer: func(addr *mgo.ServerAddr) (net.Conn, error) {
				return tls.Dial("tcp", addr.String(), &tls.Config{})
			},
		}
	} else {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  60 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
		}
	}

	success := false
	mongoDBSession, mongoDBSessionError = mgo.DialWithInfo(dialInfo)
	if mongoDBSessionError != nil {
		log.Fatal(fmt.Sprintf("Can't connect to mongo at [%s], go error: ", mongoURL), mongoDBSessionError)
	} else {
		success = true
	}

	if !success {
		os.Exit(1)
	}

	// SetSafe changes the session safety mode.
	// If the safe parameter is nil, the session is put in unsafe mode, and writes become fire-and-forget,
	// without error checking. The unsafe mode is faster since operations won't hold on waiting for a confirmation.
	// http://godoc.org/labix.org/v2/mgo#Session.SetMode.
	mongoDBSession.SetSafe(nil)

	mongoDBSession.SetMode(mgo.Monotonic, true)

	// Limit connection pool to avoid running into Request Rate Too Large on CosmosDB
	mongoDBSession.SetPoolLimit(mongoPoolLimit)
}

func ProcessOrderInMongoDB(order Order) (orderId string) {
	log.Println("ProcessOrderInMongoDB: " + order.OrderID)

	mongoDBSessionCopy := mongoDBSession.Copy()
	defer mongoDBSessionCopy.Close()

	// Get collection
	log.Println("Getting collection: " + mongoCollectionName + " in database: " + mongoDatabaseName)
	mongoDBCollection := mongoDBSessionCopy.DB(mongoDatabaseName).C(mongoCollectionName)

	// Get Document from collection
	result := Order{}
	log.Println("Looking for ", "{", "orderid:", order.OrderID, ",", "status:", "Open", "}")

	err := mongoDBCollection.Find(bson.M{"orderid": order.OrderID, "status": "Open"}).One(&result)

	if err != nil {
		log.Println("Not found (already processed) or error: ", err)
	} else {

		change := bson.M{"$set": bson.M{"status": "Processed"}}

		// Try updating the record, with retry logic
		err := try.Do(func(attempt int) (bool, error) {
			var err error
		
			err = mongoDBCollection.Update(result, change)
	
			if err != nil {
				log.Println("Error processingrecord. Will retry in 3 seconds:", err)
				  time.Sleep(3 * time.Second) // wait
			} else {
				log.Println("set status: Processed")
			}
			return attempt < 3, err
		  })
		  
		if err != nil {
			log.Println("Error updating record after retrying 3 times: ", err)
			return
		}
	}


	// Track the event for the challenge purposes
	eventTelemetry := appinsights.NewEventTelemetry("FulfillOrder: - Team Name " + teamname + " db " + db)
	eventTelemetry.Properties["team"] = teamname
	eventTelemetry.Properties["challenge"] = "4-fulfillorder"
	eventTelemetry.Properties["type"] = db
	challengeTelemetryClient.Track(eventTelemetry)
	

	// Let's place on the file system
	f, err := os.Create("/orders/" + order.OrderID + ".json")
	check(err)

	fmt.Fprintf(f, "{", "orderid:", order.OrderID, ",", "status:", "Processed", "}")

	// Issue a `Sync` to flush writes to stable storage.
	f.Sync()

	return order.OrderID
}

func check(e error) {
	if e != nil {
		log.Println("order volume not mounted")
	} else {
		// Track the event for the challenge purposes
		eventTelemetry := appinsights.NewEventTelemetry("ProcessOrder: - Team Name " + teamname + " db " + db)
		eventTelemetry.Properties["team"] = teamname
		eventTelemetry.Properties["challenge"] = "5-fileshare"
		eventTelemetry.Properties["type"] = db
		challengeTelemetryClient.Track(eventTelemetry)
	}
}
