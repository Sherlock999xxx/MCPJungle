package model

import "gorm.io/gorm"

// ManagedConfigFile tracks entities that are managed through the auto-synced config directory.
type ManagedConfigFile struct {
	gorm.Model

	EntityType string `json:"entity_type" gorm:"type:varchar(32);not null;index:idx_managed_config_entity,unique"`
	EntityName string `json:"entity_name" gorm:"type:varchar(255);not null;index:idx_managed_config_entity,unique"`
	FilePath   string `json:"file_path" gorm:"type:text;not null;index:idx_managed_config_file,unique"`
	FileHash   string `json:"file_hash" gorm:"type:varchar(128);not null"`
}
