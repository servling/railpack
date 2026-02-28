package plan

const (
	CacheTypeShared = "shared"
	CacheTypeLocked = "locked"
)

type Cache struct {
	Directory string `json:"directory,omitempty" jsonschema:"description=The directory to cache"`
	Type      string `json:"type,omitempty" jsonschema:"enum=shared,enum=locked,default=shared,description=The type of cache (either 'shared' or 'locked')"`
	Global    bool   `json:"global,omitempty" jsonschema:"description=Whether the cache should be shared globally across all services"`
}

func NewCache(directory string) *Cache {
	return &Cache{
		Directory: directory,
		Type:      CacheTypeShared,
	}
}
