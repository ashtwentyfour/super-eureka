package awspricing

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

const (
	hoursPerMonth                 = 730
	defaultDynamoDBProvisionedRCU = 5
	defaultDynamoDBProvisionedWCU = 5
	defaultDynamoDBStorageGB      = 1
	defaultDynamoDBOnDemandReads  = 1_000_000
	defaultDynamoDBOnDemandWrites = 1_000_000
	defaultS3StorageGB            = 50
	defaultSQSRequests            = 1_000_000
	defaultSNSPublishes           = 1_000_000
	defaultECRStorageGB           = 10
	defaultLambdaRequests         = 1_000_000
	defaultLambdaDurationSeconds  = 0.1
	defaultLambdaMemoryMB         = 128
	defaultEKSNodeCount           = 2
	defaultECSDesiredCount        = 1
	defaultFargateVCPU            = 0.25
	defaultFargateMemoryMB        = 512
	defaultLoadBalancerCapacity   = 1
	defaultNATDataGB              = 100
	defaultVPCEndpointDataGB      = 100
)

type Pricer struct {
	client *pricing.Client
}

type catalogProduct struct {
	Attributes map[string]string
	Dimensions []priceDimension
}

type priceDimension struct {
	Unit        string
	Description string
	USD         float64
}

func New(ctx context.Context, region string) (*Pricer, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Pricer{client: pricing.NewFromConfig(cfg)}, nil
}

func (p *Pricer) Estimate(ctx context.Context, analysis *terraform.Analysis) ([]terraform.EstimatedCost, error) {
	results := make([]terraform.EstimatedCost, 0, len(analysis.Resources))
	for _, resource := range analysis.Resources {
		estimate, err := p.estimateResource(ctx, analysis, resource)
		if err != nil {
			return nil, err
		}
		results = append(results, estimate)
	}
	return results, nil
}

func (p *Pricer) estimateResource(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	switch resource.Type {
	case "aws_instance":
		return p.estimateEC2Instance(ctx, analysis, resource)
	case "aws_ebs_volume":
		return p.estimateEBSVolume(ctx, analysis, resource)
	case "aws_db_instance":
		return p.estimateRDSInstance(ctx, analysis, resource)
	case "aws_rds_cluster_instance":
		return p.estimateRDSClusterInstance(ctx, analysis, resource)
	case "aws_dynamodb_table":
		return p.estimateDynamoDBTable(ctx, analysis, resource)
	case "aws_s3_bucket":
		return p.estimateS3Bucket(ctx, analysis, resource)
	case "aws_sqs_queue":
		return p.estimateSQSQueue(ctx, analysis, resource)
	case "aws_sns_topic":
		return p.estimateSNSTopic(ctx, analysis, resource)
	case "aws_lambda_function":
		return p.estimateLambdaFunction(ctx, analysis, resource)
	case "aws_ecr_repository":
		return p.estimateECRRepository(ctx, analysis, resource)
	default:
		return p.estimateExtendedAWSResource(ctx, analysis, resource)
	}
}

func (p *Pricer) estimateEC2Instance(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	instanceType := stringAttr(resource, "instance_type")
	if instanceType == "" {
		return unsupported(resource, "instance_type is dynamic or missing"), nil
	}

	location := pricingLocation(analysis)
	usdPerHour, err := p.lookupOnDemandPrice(ctx, "AmazonEC2", []types.Filter{
		stringFilter("location", location),
		stringFilter("instanceType", instanceType),
		stringFilter("operatingSystem", "Linux"),
		stringFilter("preInstalledSw", "NA"),
		stringFilter("tenancy", "Shared"),
		stringFilter("capacitystatus", "Used"),
	}, "Hrs")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(usdPerHour * hoursPerMonth),
		Basis:           fmt.Sprintf("%s Linux on-demand x %d hours/month", instanceType, hoursPerMonth),
		Notes:           fmt.Sprintf("Estimated using AWS Pricing location %s.", location),
		Assumptions:     nil,
	}, nil
}

func (p *Pricer) estimateEBSVolume(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	size := numberAttr(resource, "size")
	if size <= 0 {
		return unsupported(resource, "size is dynamic or missing"), nil
	}

	rawVolumeType := stringAttr(resource, "type")
	volumeType := rawVolumeType
	if volumeType == "" {
		volumeType = "gp3"
	}

	location := pricingLocation(analysis)
	productFamily := map[string]string{
		"gp2": "Storage",
		"gp3": "Storage",
		"io1": "System Operation",
		"io2": "System Operation",
		"st1": "Storage",
		"sc1": "Storage",
	}

	usdPerGBMonth, err := p.lookupOnDemandPrice(ctx, "AmazonEC2", []types.Filter{
		stringFilter("location", location),
		stringFilter("volumeType", ebsVolumeTypeName(volumeType)),
		stringFilter("productFamily", productFamily[volumeType]),
	}, "GB-Mo")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(usdPerGBMonth * size),
		Basis:           fmt.Sprintf("%s %.0f GB-month", volumeType, size),
		Notes:           fmt.Sprintf("IOPS and throughput surcharges are not included for %s.", volumeType),
		Assumptions:     conditionalAssumptions(rawVolumeType == "", "EBS volume type defaulted to gp3 because `type` was not specified."),
	}, nil
}

func (p *Pricer) estimateRDSInstance(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	instanceClass := stringAttr(resource, "instance_class")
	engine := strings.ToLower(stringAttr(resource, "engine"))
	if instanceClass == "" || engine == "" {
		return unsupported(resource, "instance_class or engine is dynamic or missing"), nil
	}

	location := pricingLocation(analysis)
	usdPerHour, err := p.lookupOnDemandPrice(ctx, "AmazonRDS", []types.Filter{
		stringFilter("location", location),
		stringFilter("instanceType", instanceClass),
		stringFilter("databaseEngine", rdsEngineName(engine)),
		stringFilter("deploymentOption", "Single-AZ"),
		stringFilter("licenseModel", "No license required"),
	}, "Hrs")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	storageGB := numberAttr(resource, "allocated_storage")
	notes := fmt.Sprintf("%s %s instance only; storage and I/O not included.", engine, instanceClass)
	if storageGB > 0 {
		notes = fmt.Sprintf("%s Allocated storage (%0.f GB) was detected but not priced yet.", notes, storageGB)
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(usdPerHour * hoursPerMonth),
		Basis:           fmt.Sprintf("%s %s x %d hours/month", engine, instanceClass, hoursPerMonth),
		Notes:           notes,
	}, nil
}

func (p *Pricer) estimateRDSClusterInstance(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	instanceClass := stringAttr(resource, "instance_class")
	engine := strings.ToLower(stringAttr(resource, "engine"))
	if engine == "" {
		engine = "aurora"
	}
	if instanceClass == "" {
		return unsupported(resource, "instance_class is dynamic or missing"), nil
	}

	location := pricingLocation(analysis)
	usdPerHour, err := p.lookupOnDemandPrice(ctx, "AmazonRDS", []types.Filter{
		stringFilter("location", location),
		stringFilter("instanceType", instanceClass),
		stringFilter("databaseEngine", rdsEngineName(engine)),
	}, "Hrs")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(usdPerHour * hoursPerMonth),
		Basis:           fmt.Sprintf("%s %s x %d hours/month", engine, instanceClass, hoursPerMonth),
		Notes:           "Aurora/cluster storage, I/O, backup, and data transfer are not included.",
	}, nil
}

func (p *Pricer) estimateDynamoDBTable(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AmazonDynamoDB", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	billingMode := strings.ToUpper(stringAttr(resource, "billing_mode"))
	if billingMode == "" {
		billingMode = "PROVISIONED"
	}

	storagePrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
		return hasAll(dim.Unit, "gb") && hasAny(dim.Description, "timed storage", "storage")
	})

	var monthly float64
	var basis []string
	var notes []string
	var assumptions []string

	switch billingMode {
	case "PAY_PER_REQUEST":
		if raw := stringAttr(resource, "billing_mode"); raw == "" {
			assumptions = append(assumptions, "DynamoDB billing_mode defaulted to PROVISIONED because it was not specified.")
		}
		readPrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
			return hasAny(dim.Description, "read request unit", "on-demand read request")
		})
		writePrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
			return hasAny(dim.Description, "write request unit", "on-demand write request")
		})

		if readPrice > 0 {
			monthly += readPrice * defaultDynamoDBOnDemandReads / 1_000_000
		}
		if writePrice > 0 {
			monthly += writePrice * defaultDynamoDBOnDemandWrites / 1_000_000
		}
		if storagePrice > 0 {
			monthly += storagePrice * defaultDynamoDBStorageGB
		}

		basis = append(basis, fmt.Sprintf("on-demand with %d reads and %d writes/month", defaultDynamoDBOnDemandReads, defaultDynamoDBOnDemandWrites))
		notes = append(notes, fmt.Sprintf("Storage assumed at %d GB-month.", defaultDynamoDBStorageGB))
		assumptions = append(assumptions,
			fmt.Sprintf("Assumed %d read requests/month for on-demand billing.", defaultDynamoDBOnDemandReads),
			fmt.Sprintf("Assumed %d write requests/month for on-demand billing.", defaultDynamoDBOnDemandWrites),
			fmt.Sprintf("Assumed %d GB-month of table storage.", defaultDynamoDBStorageGB),
		)
	default:
		readCapacity := numberAttr(resource, "read_capacity")
		writeCapacity := numberAttr(resource, "write_capacity")

		if readCapacity <= 0 {
			readCapacity = defaultDynamoDBProvisionedRCU
			assumptions = append(assumptions, fmt.Sprintf("Defaulted DynamoDB read_capacity to %d RCU because it was not specified as a literal value.", defaultDynamoDBProvisionedRCU))
		}
		if writeCapacity <= 0 {
			writeCapacity = defaultDynamoDBProvisionedWCU
			assumptions = append(assumptions, fmt.Sprintf("Defaulted DynamoDB write_capacity to %d WCU because it was not specified as a literal value.", defaultDynamoDBProvisionedWCU))
		}
		if raw := stringAttr(resource, "billing_mode"); raw == "" {
			assumptions = append(assumptions, "DynamoDB billing_mode defaulted to PROVISIONED because it was not specified.")
		}

		readPrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
			return hasAny(dim.Description, "read capacity unit-hour", "read request unit-hour", "read capacity unit")
		})
		writePrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
			return hasAny(dim.Description, "write capacity unit-hour", "write request unit-hour", "write capacity unit")
		})

		monthly += readPrice * readCapacity * hoursPerMonth
		monthly += writePrice * writeCapacity * hoursPerMonth
		if storagePrice > 0 {
			monthly += storagePrice * defaultDynamoDBStorageGB
		}

		basis = append(basis, fmt.Sprintf("provisioned %.0f RCU and %.0f WCU x %d hours/month", readCapacity, writeCapacity, hoursPerMonth))
		notes = append(notes, fmt.Sprintf("Storage assumed at %d GB-month.", defaultDynamoDBStorageGB))
		assumptions = append(assumptions, fmt.Sprintf("Assumed %d GB-month of table storage.", defaultDynamoDBStorageGB))
	}

	if strings.EqualFold(stringAttr(resource, "point_in_time_recovery"), "true") {
		notes = append(notes, "Point-in-time recovery may add charges and is not included.")
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           strings.Join(basis, "; "),
		Notes:           joinNotes(append(notes, fmt.Sprintf("Estimated using AWS Pricing location %s.", location))...),
		Assumptions:     assumptions,
	}, nil
}

func (p *Pricer) estimateS3Bucket(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AmazonS3", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	storageClass := strings.ToUpper(stringAttr(resource, "storage_class"))
	if storageClass == "" {
		storageClass = "STANDARD"
	}

	storagePrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		classHint := storageClass
		if classHint == "STANDARD" {
			return hasAny(dim.Description, "standard storage", "general purpose") ||
				hasAny(attrs["storageClass"], "standard")
		}
		return hasAny(dim.Description, strings.ToLower(classHint)) || hasAny(attrs["storageClass"], strings.ToLower(classHint))
	})

	monthly := storagePrice * defaultS3StorageGB
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%s storage with %d GB-month assumed", storageClass, defaultS3StorageGB),
		Notes:           "Request, transfer, lifecycle, and replication charges are not included.",
		Assumptions: append(
			conditionalAssumptions(stringAttr(resource, "storage_class") == "", "S3 storage_class defaulted to STANDARD because it was not specified."),
			fmt.Sprintf("Assumed %d GB-month of S3 storage.", defaultS3StorageGB),
		),
	}, nil
}

func (p *Pricer) estimateSQSQueue(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AmazonSQS", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	queueType := "standard"
	if strings.EqualFold(stringAttr(resource, "fifo_queue"), "true") {
		queueType = "fifo"
	}

	requestPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		if !hasAny(dim.Description, "request") {
			return false
		}
		if queueType == "fifo" {
			return hasAny(dim.Description, "fifo") || hasAny(attrs["queueType"], "fifo")
		}
		return !hasAny(dim.Description, "fifo")
	})

	monthly := requestPrice * defaultSQSRequests / 1_000_000
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%s queue with %d requests/month assumed", queueType, defaultSQSRequests),
		Notes:           "Payload, data transfer, and long-poll side effects are not included.",
		Assumptions: append(
			conditionalAssumptions(stringAttr(resource, "fifo_queue") == "", "SQS queue type defaulted to standard because `fifo_queue` was not specified."),
			fmt.Sprintf("Assumed %d SQS requests/month.", defaultSQSRequests),
		),
	}, nil
}

func (p *Pricer) estimateSNSTopic(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AmazonSNS", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	publishPrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
		return hasAny(dim.Description, "api request", "publish", "request")
	})

	monthly := publishPrice * defaultSNSPublishes / 1_000_000
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%d publishes/month assumed", defaultSNSPublishes),
		Notes:           "Delivery protocol charges such as SMS, email, or HTTP egress are not included.",
		Assumptions:     []string{fmt.Sprintf("Assumed %d SNS publishes/month.", defaultSNSPublishes)},
	}, nil
}

func (p *Pricer) estimateLambdaFunction(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AWSLambda", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	requestPrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
		return hasAny(dim.Description, "request") && !hasAny(dim.Description, "provisioned")
	})
	durationPrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
		return hasAny(dim.Description, "duration", "gb-second") && !hasAny(dim.Description, "provisioned")
	})

	memoryMB := numberAttr(resource, "memory_size")
	var assumptions []string
	if memoryMB <= 0 {
		memoryMB = defaultLambdaMemoryMB
		assumptions = append(assumptions, fmt.Sprintf("Lambda memory_size defaulted to %d MB because it was not specified as a literal value.", defaultLambdaMemoryMB))
	}
	assumptions = append(assumptions,
		fmt.Sprintf("Assumed %d Lambda invocations/month.", defaultLambdaRequests),
		fmt.Sprintf("Assumed %.0f ms average Lambda duration.", defaultLambdaDurationSeconds*1000),
	)

	gbSeconds := defaultLambdaRequests * defaultLambdaDurationSeconds * (memoryMB / 1024.0)
	monthly := requestPrice*defaultLambdaRequests/1_000_000 + durationPrice*gbSeconds

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%d invocations/month assumed at %.0f MB and %.0f ms average duration", defaultLambdaRequests, memoryMB, defaultLambdaDurationSeconds*1000),
		Notes:           "Provisioned concurrency, free tier offsets, logs, and network transfer are not included.",
		Assumptions:     assumptions,
	}, nil
}

func (p *Pricer) estimateECRRepository(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	location := pricingLocation(analysis)
	catalog, err := p.loadCatalog(ctx, "AmazonECR", location)
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	storagePrice := firstMatchingPrice(catalog, func(_ map[string]string, dim priceDimension) bool {
		return hasAll(dim.Unit, "gb") && hasAny(dim.Description, "storage")
	})

	monthly := storagePrice * defaultECRStorageGB
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%d GB image storage assumed", defaultECRStorageGB),
		Notes:           "Image scan and cross-region transfer charges are not included.",
		Assumptions:     []string{fmt.Sprintf("Assumed %d GB of ECR image storage.", defaultECRStorageGB)},
	}, nil
}

func (p *Pricer) estimateExtendedAWSResource(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	switch {
	case resource.Type == "aws_ecs_cluster" || resource.Type == "aws_ecs_task_definition":
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Container control-plane resource",
			Notes:           "No direct recurring charge is typically attached to this resource by itself; task runtime, EC2 capacity, logs, and networking may still incur cost.",
		}, nil
	case resource.Type == "aws_ecs_service":
		return p.estimateECSService(ctx, analysis, resource)
	case resource.Type == "aws_eks_cluster":
		return p.estimateEKSCluster(ctx, analysis, resource)
	case resource.Type == "aws_eks_node_group":
		return p.estimateEKSNodeGroup(ctx, analysis, resource)
	case resource.Type == "aws_eks_fargate_profile":
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Needs workload assumptions",
			Notes:           "EKS Fargate profiles define scheduling scope, but monthly cost depends on pod vCPU, memory, and runtime assumptions that are not encoded directly in the profile.",
		}, nil
	case resource.Type == "aws_nat_gateway":
		return p.estimateNATGateway(ctx, analysis, resource)
	case resource.Type == "aws_vpc_endpoint":
		return p.estimateVPCEndpoint(ctx, analysis, resource)
	case strings.HasPrefix(resource.Type, "aws_lb_"), strings.HasPrefix(resource.Type, "aws_alb_"), strings.HasPrefix(resource.Type, "aws_elb_"):
		return p.estimateLoadBalancer(ctx, analysis, resource)
	case resource.Type == "aws_elasticache_cluster" || resource.Type == "aws_elasticache_replication_group":
		return p.estimateElastiCache(ctx, analysis, resource)
	case resource.Type == "aws_opensearch_domain" || resource.Type == "aws_elasticsearch_domain":
		return p.estimateOpenSearchDomain(ctx, analysis, resource)
	case resource.Type == "aws_redshift_cluster":
		return p.estimateRedshiftCluster(ctx, analysis, resource)
	case resource.Type == "aws_docdb_cluster_instance":
		return p.estimateDocDBClusterInstance(ctx, analysis, resource)
	case resource.Type == "aws_docdb_cluster":
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Cluster storage not estimated",
			Notes:           "DocumentDB cluster instances, storage, I/O, and backup charges are estimated separately or require additional assumptions.",
		}, nil
	case resource.Type == "aws_memorydb_cluster":
		return p.estimateMemoryDBCluster(ctx, analysis, resource)
	default:
		return p.estimateGenericAWSResource(resource), nil
	}
}

func (p *Pricer) estimateECSService(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	launchType := strings.ToUpper(stringAttr(resource, "launch_type"))
	if launchType == "" {
		launchType = "EC2"
	}
	if launchType != "FARGATE" {
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "ECS service on EC2 capacity",
			Notes:           "No separate ECS service fee is estimated for EC2 launch type; EC2 instances, EBS, load balancers, logs, and data transfer should be costed separately.",
			Assumptions:     conditionalAssumptions(stringAttr(resource, "launch_type") == "", "ECS launch_type defaulted to EC2 because it was not specified."),
		}, nil
	}

	desiredCount := numberAttr(resource, "desired_count")
	assumptions := []string{}
	if desiredCount <= 0 {
		desiredCount = float64(defaultECSDesiredCount)
		assumptions = append(assumptions, fmt.Sprintf("Assumed %d running ECS task for the service because desired_count was not specified as a literal value.", defaultECSDesiredCount))
	}

	taskDef := referencedResource(analysis, resource.Attributes["task_definition"].Raw)
	vcpu := float64(defaultFargateVCPU)
	memoryMB := float64(defaultFargateMemoryMB)
	if taskDef != nil {
		if value := numberAttr(*taskDef, "cpu"); value > 0 {
			vcpu = value / 1024.0
		}
		if value := numberAttr(*taskDef, "memory"); value > 0 {
			memoryMB = value
		}
	}
	if taskDef == nil {
		assumptions = append(assumptions, fmt.Sprintf("Assumed %.2f vCPU and %d MB per Fargate task because the referenced task definition could not be resolved statically.", defaultFargateVCPU, defaultFargateMemoryMB))
	}

	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonECS", "AWSFargate")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	cpuPrice, memPrice := fargateRates(catalog)
	monthly := cpuPrice*vcpu*desiredCount*hoursPerMonth + memPrice*(memoryMB/1024.0)*desiredCount*hoursPerMonth

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("Fargate service with %.2f vCPU, %.0f MB, and %.0f tasks x %d hours/month", vcpu, memoryMB, desiredCount, hoursPerMonth),
		Notes:           "Ephemeral storage beyond the default allocation, load balancers, logs, public IPv4, and data transfer are not included.",
		Assumptions: append(assumptions,
			"Assumed Linux/x86 Fargate pricing.",
			"Assumed tasks run continuously for the full month.",
		),
	}, nil
}

func (p *Pricer) estimateEKSCluster(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonEKS")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	clusterPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "hrs") && (hasAny(dim.Description, "cluster") || hasAny(attrs["group"], "cluster"))
	})

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(clusterPrice * hoursPerMonth),
		Basis:           fmt.Sprintf("EKS control plane x %d hours/month", hoursPerMonth),
		Notes:           "Worker node compute, EBS, load balancers, and cross-AZ/network transfer are not included here.",
		Assumptions:     []string{"Assumed standard EKS Kubernetes version support pricing rather than extended support or add-on capabilities."},
	}, nil
}

func (p *Pricer) estimateEKSNodeGroup(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	instanceType := firstString(listAttr(resource, "instance_types"))
	if instanceType == "" {
		return unsupported(resource, "instance_types is dynamic or missing"), nil
	}

	desiredSize := nestedNumberAttr(resource, "scaling_config", "desired_size")
	assumptions := []string{}
	if desiredSize <= 0 {
		desiredSize = defaultEKSNodeCount
		assumptions = append(assumptions, fmt.Sprintf("Assumed %d EKS worker nodes because scaling_config.desired_size was not specified as a literal value.", defaultEKSNodeCount))
	}

	location := pricingLocation(analysis)
	usdPerHour, err := p.lookupOnDemandPrice(ctx, "AmazonEC2", []types.Filter{
		stringFilter("location", location),
		stringFilter("instanceType", instanceType),
		stringFilter("operatingSystem", "Linux"),
		stringFilter("preInstalledSw", "NA"),
		stringFilter("tenancy", "Shared"),
		stringFilter("capacitystatus", "Used"),
	}, "Hrs")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(usdPerHour * desiredSize * hoursPerMonth),
		Basis:           fmt.Sprintf("%s x %.0f EKS worker nodes x %d hours/month", instanceType, desiredSize, hoursPerMonth),
		Notes:           "EBS volumes, public IPv4, load balancers, and EKS control-plane fees are not included here.",
		Assumptions:     assumptions,
	}, nil
}

func (p *Pricer) estimateLoadBalancer(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AWSELB")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	lbType := loadBalancerType(resource)
	hourlyPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "hrs") && matchesLoadBalancerDimension(lbType, attrs, dim, false)
	})
	capacityPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && matchesLoadBalancerDimension(lbType, attrs, dim, true)
	})

	monthly := hourlyPrice*hoursPerMonth + capacityPrice*defaultLoadBalancerCapacity*hoursPerMonth
	assumptions := []string{fmt.Sprintf("Assumed %d %s capacity unit-hour for the month.", defaultLoadBalancerCapacity, loadBalancerCapacityUnit(lbType))}
	if lbType == "classic" {
		assumptions = nil
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%s load balancer x %d hours/month", lbType, hoursPerMonth),
		Notes:           "Data transfer and target service usage are not included.",
		Assumptions:     assumptions,
	}, nil
}

func (p *Pricer) estimateNATGateway(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonVPC")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	hourlyPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "hrs") && (hasAny(dim.Description, "nat gateway") || hasAny(attrs["usagetype"], "natgateway-hour"))
	})
	dataPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "gb") && (hasAny(dim.Description, "nat gateway", "data processed") || hasAny(attrs["usagetype"], "natgateway-bytes"))
	})

	monthly := hourlyPrice*hoursPerMonth + dataPrice*defaultNATDataGB
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("NAT gateway x %d hours/month", hoursPerMonth),
		Notes:           "Cross-AZ transfer and attached public IPv4 charges are not included.",
		Assumptions:     []string{fmt.Sprintf("Assumed %d GB/month of NAT gateway data processing.", defaultNATDataGB)},
	}, nil
}

func (p *Pricer) estimateVPCEndpoint(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	endpointType := strings.ToUpper(stringAttr(resource, "vpc_endpoint_type"))
	if endpointType == "" {
		endpointType = "GATEWAY"
	}
	if endpointType == "GATEWAY" {
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Gateway endpoint",
			Notes:           "Gateway VPC endpoints typically do not have a direct hourly endpoint charge; transfer and downstream service charges may still apply.",
			Assumptions:     conditionalAssumptions(stringAttr(resource, "vpc_endpoint_type") == "", "VPC endpoint type defaulted to Gateway because it was not specified."),
		}, nil
	}

	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonVPC")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	endpointCount := float64(len(listAttr(resource, "subnet_ids")))
	if endpointCount <= 0 {
		endpointCount = 1
	}

	hourlyPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "hrs") && (hasAny(dim.Description, "vpc endpoint") || hasAny(attrs["usagetype"], "vpce"))
	})
	dataPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 && hasAny(dim.Unit, "gb") && (hasAny(dim.Description, "data processed") || hasAny(attrs["usagetype"], "vpce"))
	})

	monthly := hourlyPrice*endpointCount*hoursPerMonth + dataPrice*defaultVPCEndpointDataGB
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%s VPC endpoint across %.0f subnet attachment(s)", strings.ToLower(endpointType), endpointCount),
		Notes:           "PrivateLink endpoint service/provider charges are not included.",
		Assumptions: []string{
			fmt.Sprintf("Assumed %.0f attached subnet/AZ endpoint-hour for the month.", endpointCount),
			fmt.Sprintf("Assumed %d GB/month of interface endpoint data processing.", defaultVPCEndpointDataGB),
		},
	}, nil
}

func (p *Pricer) estimateElastiCache(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	nodeType := stringAttr(resource, "node_type")
	if nodeType == "" {
		return unsupported(resource, "node_type is dynamic or missing"), nil
	}

	nodeCount := elasticacheNodeCount(resource)
	if nodeCount <= 0 {
		nodeCount = 1
	}
	engine := strings.ToLower(stringAttr(resource, "engine"))
	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonElastiCache")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	price := instanceHourlyPrice(catalog, nodeType, engine)

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(price * nodeCount * hoursPerMonth),
		Basis:           fmt.Sprintf("%s x %.0f cache node(s) x %d hours/month", nodeType, nodeCount, hoursPerMonth),
		Notes:           "Backup storage, data transfer, and serverless/request-based cache modes are not included.",
	}, nil
}

func (p *Pricer) estimateOpenSearchDomain(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	dataType := nestedStringAttr(resource, "cluster_config", "instance_type")
	if dataType == "" {
		return unsupported(resource, "cluster_config.instance_type is dynamic or missing"), nil
	}
	dataCount := nestedNumberAttr(resource, "cluster_config", "instance_count")
	if dataCount <= 0 {
		dataCount = 1
	}

	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonOpenSearchService", "AmazonES")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}

	monthly := instanceHourlyPrice(catalog, dataType, "") * dataCount * hoursPerMonth
	if strings.EqualFold(fmt.Sprint(nestedAttr(resource, "cluster_config", "dedicated_master_enabled")), "true") {
		masterType := nestedStringAttr(resource, "cluster_config", "dedicated_master_type")
		masterCount := nestedNumberAttr(resource, "cluster_config", "dedicated_master_count")
		if masterType != "" && masterCount > 0 {
			monthly += instanceHourlyPrice(catalog, masterType, "") * masterCount * hoursPerMonth
		}
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(monthly),
		Basis:           fmt.Sprintf("%s OpenSearch data nodes x %.0f x %d hours/month", dataType, dataCount, hoursPerMonth),
		Notes:           "EBS/gp3 storage, snapshots, UltraWarm/cold tiers, and data transfer are not included.",
	}, nil
}

func (p *Pricer) estimateRedshiftCluster(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	nodeType := stringAttr(resource, "node_type")
	if nodeType == "" {
		return unsupported(resource, "node_type is dynamic or missing"), nil
	}
	nodeCount := numberAttr(resource, "number_of_nodes")
	if nodeCount <= 0 {
		nodeCount = 1
	}

	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonRedshift")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	price := instanceHourlyPrice(catalog, nodeType, "")

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(price * nodeCount * hoursPerMonth),
		Basis:           fmt.Sprintf("%s x %.0f Redshift node(s) x %d hours/month", nodeType, nodeCount, hoursPerMonth),
		Notes:           "Managed storage, Spectrum, concurrency scaling, backups, and data transfer are not included.",
	}, nil
}

func (p *Pricer) estimateDocDBClusterInstance(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	instanceClass := stringAttr(resource, "instance_class")
	if instanceClass == "" {
		return unsupported(resource, "instance_class is dynamic or missing"), nil
	}
	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonDocDB", "AmazonRDS")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	price := instanceHourlyPrice(catalog, instanceClass, "docdb")

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(price * hoursPerMonth),
		Basis:           fmt.Sprintf("%s DocumentDB instance x %d hours/month", instanceClass, hoursPerMonth),
		Notes:           "Cluster storage, I/O, backups, and data transfer are not included.",
	}, nil
}

func (p *Pricer) estimateMemoryDBCluster(ctx context.Context, analysis *terraform.Analysis, resource terraform.Resource) (terraform.EstimatedCost, error) {
	nodeType := stringAttr(resource, "node_type")
	if nodeType == "" {
		return unsupported(resource, "node_type is dynamic or missing"), nil
	}
	shards := numberAttr(resource, "num_shards")
	if shards <= 0 {
		shards = 1
	}
	replicas := numberAttr(resource, "num_replicas_per_shard")
	nodeCount := shards * (1 + replicas)

	catalog, err := p.loadCatalogAny(ctx, pricingLocation(analysis), "AmazonMemoryDB")
	if err != nil {
		return terraform.EstimatedCost{}, err
	}
	price := instanceHourlyPrice(catalog, nodeType, "")

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      round2(price * nodeCount * hoursPerMonth),
		Basis:           fmt.Sprintf("%s x %.0f MemoryDB node(s) x %d hours/month", nodeType, nodeCount, hoursPerMonth),
		Notes:           "Snapshot storage, data transfer, and serverless/request-based modes are not included.",
	}, nil
}

func (p *Pricer) estimateGenericAWSResource(resource terraform.Resource) terraform.EstimatedCost {
	if !strings.HasPrefix(resource.Type, "aws_") {
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Non-AWS resource",
			Notes:           "This estimator only handles Terraform resources backed by AWS.",
			Assumptions:     nil,
		}
	}

	if isLikelyFreeControlPlaneResource(resource.Type) {
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Control-plane resource",
			Notes:           "No direct recurring infrastructure charge is usually attached to this resource type; dependent service usage may still incur cost.",
			Assumptions:     nil,
		}
	}

	if service := inferAWSServiceFromTerraformType(resource.Type); service != "" {
		return terraform.EstimatedCost{
			ResourceAddress: resource.Address(),
			MonthlyUSD:      0,
			Basis:           "Best-effort classification only",
			Notes:           fmt.Sprintf("Mapped %s to AWS service %s, but this resource needs service-specific usage assumptions or a bespoke pricing rule before a reliable monthly estimate can be produced.", resource.Type, service),
			Assumptions:     nil,
		}
	}

	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      0,
		Basis:           "Unknown AWS pricing model",
		Notes:           fmt.Sprintf("No service mapping is implemented yet for %s.", resource.Type),
		Assumptions:     nil,
	}
}

func (p *Pricer) loadCatalogAny(ctx context.Context, location string, serviceCodes ...string) ([]catalogProduct, error) {
	var lastErr error
	for _, serviceCode := range serviceCodes {
		catalog, err := p.loadCatalog(ctx, serviceCode, location)
		if err == nil && len(catalog) > 0 {
			return catalog, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no pricing catalog returned for service codes %s", strings.Join(serviceCodes, ", "))
}

func (p *Pricer) loadCatalog(ctx context.Context, serviceCode, location string) ([]catalogProduct, error) {
	resp, err := p.client.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String(serviceCode),
		Filters: []types.Filter{
			stringFilter("location", location),
		},
		MaxResults:    aws.Int32(100),
		FormatVersion: aws.String("aws_v1"),
	})
	if err != nil {
		return nil, fmt.Errorf("get pricing products for %s: %w", serviceCode, err)
	}

	products := make([]catalogProduct, 0, len(resp.PriceList))
	for _, priceList := range resp.PriceList {
		product, ok := parseCatalogProduct(priceList)
		if ok {
			products = append(products, product)
		}
	}
	return products, nil
}

func (p *Pricer) lookupOnDemandPrice(ctx context.Context, serviceCode string, filters []types.Filter, unit string) (float64, error) {
	resp, err := p.client.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode:   aws.String(serviceCode),
		Filters:       filters,
		MaxResults:    aws.Int32(20),
		FormatVersion: aws.String("aws_v1"),
	})
	if err != nil {
		return 0, fmt.Errorf("get pricing products for %s: %w", serviceCode, err)
	}

	for _, priceList := range resp.PriceList {
		product, ok := parseCatalogProduct(priceList)
		if !ok {
			continue
		}
		for _, dim := range product.Dimensions {
			if dim.Unit == unit && dim.USD > 0 {
				return dim.USD, nil
			}
		}
	}

	return 0, fmt.Errorf("no pricing match found for service=%s unit=%s", serviceCode, unit)
}

func parseCatalogProduct(raw string) (catalogProduct, bool) {
	var payload struct {
		Product struct {
			Attributes map[string]string `json:"attributes"`
		} `json:"product"`
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string            `json:"unit"`
					Description  string            `json:"description"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}

	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return catalogProduct{}, false
	}

	product := catalogProduct{
		Attributes: payload.Product.Attributes,
	}

	for _, term := range payload.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			value, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
			if err != nil {
				continue
			}
			product.Dimensions = append(product.Dimensions, priceDimension{
				Unit:        dim.Unit,
				Description: dim.Description,
				USD:         value,
			})
		}
	}

	return product, true
}

func firstMatchingPrice(products []catalogProduct, match func(attrs map[string]string, dim priceDimension) bool) float64 {
	best := 0.0
	for _, product := range products {
		for _, dim := range product.Dimensions {
			if !match(product.Attributes, dim) || dim.USD <= 0 {
				continue
			}
			if best == 0 || dim.USD < best {
				best = dim.USD
			}
		}
	}
	return best
}

func pricingLocation(analysis *terraform.Analysis) string {
	region := "us-east-1"
	if analysis.Backend != nil && analysis.Backend.Region != "" {
		region = analysis.Backend.Region
	}

	locations := map[string]string{
		"us-east-1":    "US East (N. Virginia)",
		"us-east-2":    "US East (Ohio)",
		"us-west-1":    "US West (N. California)",
		"us-west-2":    "US West (Oregon)",
		"ca-central-1": "Canada (Central)",
		"eu-west-1":    "EU (Ireland)",
	}
	if location, ok := locations[region]; ok {
		return location
	}
	return "US East (N. Virginia)"
}

func ebsVolumeTypeName(volumeType string) string {
	names := map[string]string{
		"gp2": "General Purpose",
		"gp3": "General Purpose",
		"io1": "Provisioned IOPS",
		"io2": "Provisioned IOPS",
		"st1": "Throughput Optimized HDD",
		"sc1": "Cold HDD",
	}
	if name, ok := names[volumeType]; ok {
		return name
	}
	return volumeType
}

func rdsEngineName(engine string) string {
	switch engine {
	case "postgres":
		return "PostgreSQL"
	case "mysql":
		return "MySQL"
	case "mariadb":
		return "MariaDB"
	default:
		return strings.ToUpper(engine)
	}
}

func stringFilter(field, value string) types.Filter {
	return types.Filter{
		Field: aws.String(field),
		Type:  types.FilterTypeTermMatch,
		Value: aws.String(value),
	}
}

func stringAttr(resource terraform.Resource, key string) string {
	value, ok := resource.Attributes[key]
	if !ok {
		return ""
	}
	if s, ok := value.Literal.(string); ok {
		return s
	}
	return ""
}

func listAttr(resource terraform.Resource, key string) []any {
	value, ok := resource.Attributes[key]
	if !ok {
		return nil
	}
	if items, ok := value.Literal.([]any); ok {
		return items
	}
	return nil
}

func numberAttr(resource terraform.Resource, key string) float64 {
	value, ok := resource.Attributes[key]
	if !ok {
		return 0
	}
	switch v := value.Literal.(type) {
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	case float64:
		return v
	default:
		return 0
	}
}

func nestedAttr(resource terraform.Resource, blockName, key string) any {
	value, ok := resource.Attributes[blockName]
	if !ok {
		return nil
	}
	switch block := value.Literal.(type) {
	case map[string]any:
		return block[key]
	case []any:
		if len(block) == 0 {
			return nil
		}
		if first, ok := block[0].(map[string]any); ok {
			return first[key]
		}
	}
	return nil
}

func nestedNumberAttr(resource terraform.Resource, blockName, key string) float64 {
	switch v := nestedAttr(resource, blockName, key).(type) {
	case float64:
		return v
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	default:
		return 0
	}
}

func nestedStringAttr(resource terraform.Resource, blockName, key string) string {
	switch v := nestedAttr(resource, blockName, key).(type) {
	case string:
		return v
	default:
		return ""
	}
}

func firstString(items []any) string {
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func referencedResource(analysis *terraform.Analysis, raw string) *terraform.Resource {
	resourceType, name := parseResourceReference(raw)
	if resourceType == "" || name == "" {
		return nil
	}
	for i := range analysis.Resources {
		if analysis.Resources[i].Type == resourceType && analysis.Resources[i].Name == name {
			return &analysis.Resources[i]
		}
	}
	return nil
}

func parseResourceReference(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"")
	if raw == "" {
		return "", ""
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "aws_") {
		return "", ""
	}
	return parts[0], parts[1]
}

func fargateRates(catalog []catalogProduct) (float64, float64) {
	cpuPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 &&
			(hasAny(dim.Description, "vcpu", "cpu") || hasAny(attrs["groupDescription"], "cpu")) &&
			!hasAny(dim.Description, "storage", "spot", "windows", "arm")
	})
	memPrice := firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		return dim.USD > 0 &&
			(hasAny(dim.Description, "memory", "gb") || hasAny(attrs["groupDescription"], "memory")) &&
			!hasAny(dim.Description, "storage", "spot", "windows", "arm")
	})
	return cpuPrice, memPrice
}

func loadBalancerType(resource terraform.Resource) string {
	switch {
	case resource.Type == "aws_elb":
		return "classic"
	case resource.Type == "aws_lb":
		if kind := strings.ToLower(stringAttr(resource, "load_balancer_type")); kind != "" {
			return kind
		}
	case strings.HasPrefix(resource.Type, "aws_alb_"), strings.HasPrefix(resource.Type, "aws_lb_listener"), strings.HasPrefix(resource.Type, "aws_lb_target_group"):
		return "application"
	}
	return "application"
}

func loadBalancerCapacityUnit(lbType string) string {
	if lbType == "network" {
		return "NLCU"
	}
	return "LCU"
}

func matchesLoadBalancerDimension(lbType string, attrs map[string]string, dim priceDimension, capacity bool) bool {
	description := strings.ToLower(dim.Description)
	if capacity {
		switch lbType {
		case "network":
			return hasAny(description, "nlcu") || hasAny(attrs["groupDescription"], "nlcu")
		case "classic":
			return false
		default:
			return hasAny(description, "lcu") || hasAny(attrs["groupDescription"], "lcu")
		}
	}

	if !hasAny(description, "load balancer", "hour") && !hasAny(attrs["groupDescription"], "load balancer") {
		return false
	}
	switch lbType {
	case "network":
		return hasAny(description, "network") || hasAny(attrs["usagetype"], "network")
	case "classic":
		return hasAny(description, "classic") || hasAny(attrs["usagetype"], "loadbalancerusage")
	default:
		return hasAny(description, "application") || hasAny(attrs["usagetype"], "application")
	}
}

func elasticacheNodeCount(resource terraform.Resource) float64 {
	if count := numberAttr(resource, "num_cache_nodes"); count > 0 {
		return count
	}
	if count := numberAttr(resource, "num_cache_clusters"); count > 0 {
		return count
	}
	groups := nestedNumberAttr(resource, "cluster_mode", "num_node_groups")
	if groups <= 0 {
		groups = 1
	}
	replicas := nestedNumberAttr(resource, "cluster_mode", "replicas_per_node_group")
	if groups > 0 {
		return groups * (1 + replicas)
	}
	return 0
}

func instanceHourlyPrice(catalog []catalogProduct, instanceType, engineHint string) float64 {
	instanceType = strings.ToLower(instanceType)
	engineHint = strings.ToLower(engineHint)
	return firstMatchingPrice(catalog, func(attrs map[string]string, dim priceDimension) bool {
		if dim.USD <= 0 || !hasAny(dim.Unit, "hrs") {
			return false
		}
		if !matchesInstanceType(attrs, dim.Description, instanceType) {
			return false
		}
		if engineHint == "" {
			return true
		}
		for _, value := range attrs {
			if hasAny(value, engineHint) {
				return true
			}
		}
		return hasAny(dim.Description, engineHint)
	})
}

func matchesInstanceType(attrs map[string]string, description, instanceType string) bool {
	if instanceType == "" {
		return false
	}
	for key, value := range attrs {
		if hasAny(strings.ToLower(key), "instance", "node", "cache") && strings.EqualFold(value, instanceType) {
			return true
		}
	}
	return hasAny(description, instanceType)
}

func unsupported(resource terraform.Resource, reason string) terraform.EstimatedCost {
	return terraform.EstimatedCost{
		ResourceAddress: resource.Address(),
		MonthlyUSD:      0,
		Basis:           "Insufficient static data",
		Notes:           "Automatic estimate unsupported because " + reason + ".",
	}
}

func inferAWSServiceFromTerraformType(resourceType string) string {
	switch {
	case strings.HasPrefix(resourceType, "aws_dynamodb_"):
		return "AmazonDynamoDB"
	case strings.HasPrefix(resourceType, "aws_lambda_"):
		return "AWSLambda"
	case strings.HasPrefix(resourceType, "aws_s3_"):
		return "AmazonS3"
	case strings.HasPrefix(resourceType, "aws_sqs_"):
		return "AmazonSQS"
	case strings.HasPrefix(resourceType, "aws_sns_"):
		return "AmazonSNS"
	case strings.HasPrefix(resourceType, "aws_ecr_"):
		return "AmazonECR"
	case strings.HasPrefix(resourceType, "aws_rds_"), strings.HasPrefix(resourceType, "aws_db_"):
		return "AmazonRDS"
	case strings.HasPrefix(resourceType, "aws_ecs_"):
		return "AmazonECS"
	case strings.HasPrefix(resourceType, "aws_eks_"):
		return "AmazonEKS"
	case strings.HasPrefix(resourceType, "aws_elasticache_"):
		return "AmazonElastiCache"
	case strings.HasPrefix(resourceType, "aws_opensearch_"):
		return "AmazonOpenSearch"
	case strings.HasPrefix(resourceType, "aws_redshift_"):
		return "AmazonRedshift"
	case strings.HasPrefix(resourceType, "aws_cloudfront_"):
		return "AmazonCloudFront"
	case strings.HasPrefix(resourceType, "aws_api_gateway_"), strings.HasPrefix(resourceType, "aws_apigatewayv2_"):
		return "AmazonApiGateway"
	case strings.HasPrefix(resourceType, "aws_lb_"), strings.HasPrefix(resourceType, "aws_alb_"), strings.HasPrefix(resourceType, "aws_elb_"):
		return "AWSELB"
	case strings.HasPrefix(resourceType, "aws_nat_gateway"):
		return "AmazonVPC"
	default:
		return ""
	}
}

func isLikelyFreeControlPlaneResource(resourceType string) bool {
	freePrefixes := []string{
		"aws_iam_",
		"aws_security_group",
		"aws_route_table",
		"aws_route",
		"aws_subnet",
		"aws_vpc",
		"aws_internet_gateway",
		"aws_network_acl",
		"aws_cloudwatch_metric_alarm",
	}
	for _, prefix := range freePrefixes {
		if strings.HasPrefix(resourceType, prefix) {
			return true
		}
	}
	return false
}

func hasAny(value string, needles ...string) bool {
	value = strings.ToLower(value)
	for _, needle := range needles {
		if strings.Contains(value, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func hasAll(value string, needles ...string) bool {
	value = strings.ToLower(value)
	for _, needle := range needles {
		if !strings.Contains(value, strings.ToLower(needle)) {
			return false
		}
	}
	return true
}

func joinNotes(notes ...string) string {
	filtered := notes[:0]
	for _, note := range notes {
		if strings.TrimSpace(note) != "" {
			filtered = append(filtered, note)
		}
	}
	return strings.Join(filtered, " ")
}

func conditionalAssumptions(include bool, values ...string) []string {
	if !include {
		return nil
	}
	var assumptions []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			assumptions = append(assumptions, value)
		}
	}
	return assumptions
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
