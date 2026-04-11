package domainmodel

import "time"

type SecurityGroupRuleDirection string
type SecurityGroupRuleProtocol string

const (
	SecurityGroupRuleDirectionIngress SecurityGroupRuleDirection = "ingress"
	SecurityGroupRuleDirectionEgress  SecurityGroupRuleDirection = "egress"

	SecurityGroupRuleProtocolTCP  SecurityGroupRuleProtocol = "tcp"
	SecurityGroupRuleProtocolUDP  SecurityGroupRuleProtocol = "udp"
	SecurityGroupRuleProtocolICMP SecurityGroupRuleProtocol = "icmp"
	SecurityGroupRuleProtocolAll  SecurityGroupRuleProtocol = "all"
)

type SecurityGroupRule struct {
	ID                    string                     `json:"id" db:"id"`
	SecurityGroupID       string                     `json:"security_group_id" db:"security_group_id"`
	Direction             SecurityGroupRuleDirection `json:"direction" db:"direction"`
	Protocol              SecurityGroupRuleProtocol  `json:"protocol" db:"protocol"`
	FromPort              *int                       `json:"from_port,omitempty" db:"from_port"`
	ToPort                *int                       `json:"to_port,omitempty" db:"to_port"`
	SourceCIDR            *string                    `json:"source_cidr,omitempty" db:"source_cidr"`
	SourceSecurityGroupID *string                    `json:"source_security_group_id,omitempty" db:"source_security_group_id"`
	Description           *string                    `json:"description,omitempty" db:"description"`
	CreatedAt             time.Time                  `json:"created_at" db:"created_at"`
}
