package main

import (
    "context"
    "fmt"
    "net/url"
    "os"
    "regexp"
    "strings"
    "time"

    "github.com/jomei/notionapi"
)

type NotionDao struct {
    feedDatabaseId    notionapi.DatabaseID
    contentDatabaseId notionapi.DatabaseID
    client            *notionapi.Client
}

// ConstructNotionDaoFromEnv given environment variables: NOTION_RSS_KEY,
// NOTION_RSS_CONTENT_DATABASE_ID, NOTION_RSS_FEEDS_DATABASE_ID
func ConstructNotionDaoFromEnv() (*NotionDao, error) {
    integrationKey, exists := os.LookupEnv("NOTION_RSS_KEY")
    if !exists {
        return &NotionDao{}, fmt.Errorf("`NOTION_RSS_KEY` not set")
    }

    contentDatabaseId, exists := os.LookupEnv("NOTION_RSS_CONTENT_DATABASE_ID")
    if !exists {
        return &NotionDao{}, fmt.Errorf("`NOTION_RSS_CONTENT_DATABASE_ID` not set")
    }

    feedDatabaseId, exists := os.LookupEnv("NOTION_RSS_FEEDS_DATABASE_ID")
    if !exists {
        return &NotionDao{}, fmt.Errorf("`NOTION_RSS_FEEDS_DATABASE_ID` not set")
    }

    return ConstructNotionDao(feedDatabaseId, contentDatabaseId, integrationKey), nil
}

func ConstructNotionDao(feedDatabaseId string, contentDatabaseId string, integrationKey string) *NotionDao {
    return &NotionDao{
        feedDatabaseId:    notionapi.DatabaseID(feedDatabaseId),
        contentDatabaseId: notionapi.DatabaseID(contentDatabaseId),
        client:            notionapi.NewClient(notionapi.Token(integrationKey)),
    }
}

// GetOldUnstarredRSSItems that were created strictly before olderThan and are not starred.
func (dao NotionDao) GetOldUnstarredRSSItems(olderThan time.Time) []notionapi.Page {
    resp, err := dao.client.Database.Query(context.TODO(), dao.contentDatabaseId, &notionapi.DatabaseQueryRequest{
        Filter: (notionapi.AndCompoundFilter)([]notionapi.Filter{

            // Use `Created`, not `Published` as to avoid deleting cold-started RSS feeds.
            notionapi.PropertyFilter{
                Property: "Created",
                Date: &notionapi.DateFilterCondition{
                    Before: (*notionapi.Date)(&olderThan),
                },
            },
            notionapi.PropertyFilter{
                Property: "Starred",
                Checkbox: &notionapi.CheckboxFilterCondition{
                    Equals:       false,
                    DoesNotEqual: true,
                },
            },
        }),
        // TODO: pagination
        //StartCursor:    "",
        //PageSize:       0,
    })
    if err != nil {
        fmt.Printf("error occurred in GetOldUnstarredRSSItems. Error: %s\n", err.Error())
        return []notionapi.Page{}
    }
    return resp.Results
}

func (dao NotionDao) GetOldUnstarredRSSItemIds(olderThan time.Time) []notionapi.PageID {
    pages := dao.GetOldUnstarredRSSItems(olderThan)
    result := make([]notionapi.PageID, len(pages))
    for i, page := range pages {
        result[i] = notionapi.PageID(page.ID)
    }
    return result
}

// ArchivePages for a list of pageIds. Will archive each page even if other pages fail.
func (dao *NotionDao) ArchivePages(pageIds []notionapi.PageID) error {
    failedCount := 0
    for _, p := range pageIds {
        _, err := dao.client.Page.Update(
            context.TODO(),
            p,
            &notionapi.PageUpdateRequest{
                Archived:   true,
                Properties: notionapi.Properties{}, // Must be provided, even if empty
            },
        )
        if err != nil {
            fmt.Printf("Failed to archive page: %s. Error: %s\n", p.String(), err.Error())
            failedCount++
        }
    }
    if failedCount > 0 {
        return fmt.Errorf("failed to archive %d pages", failedCount)
    }
    return nil
}

// GetEnabledRssFeeds from the Feed Database. Results filtered on property "Enabled"=true
func (dao *NotionDao) GetEnabledRssFeeds() chan *FeedDatabaseItem {
    rssFeeds := make(chan *FeedDatabaseItem)

    go func(dao *NotionDao, output chan *FeedDatabaseItem) {
        defer close(output)

        req := &notionapi.DatabaseQueryRequest{
            Filter: notionapi.PropertyFilter{
                Property: "Enabled",
                Checkbox: &notionapi.CheckboxFilterCondition{
                    Equals: true,
                },
            },
        }

        //TODO: Get multi-page pagination results from resp.HasMore
        resp, err := dao.client.Database.Query(context.Background(), dao.feedDatabaseId, req)
        if err != nil {
            return
        }
        for _, r := range resp.Results {
            feed, err := GetRssFeedFromDatabaseObject(&r)
            if err == nil {
                rssFeeds <- feed
            }
        }
    }(dao, rssFeeds)
    return rssFeeds
}

func GetRssFeedFromDatabaseObject(p *notionapi.Page) (*FeedDatabaseItem, error) {
    if p.Properties["Link"] == nil || p.Properties["Title"] == nil {
        return &FeedDatabaseItem{}, fmt.Errorf("notion page is expected to have `Link` and `Title` properties. Properties: %s", p.Properties)
    }
    urlProperty := p.Properties["Link"].(*notionapi.URLProperty).URL
    rssUrl, err := url.Parse(urlProperty)
    if err != nil {
        return &FeedDatabaseItem{}, err
    }

    nameRichTexts := p.Properties["Title"].(*notionapi.TitleProperty).Title
    if len(nameRichTexts) == 0 {
        return &FeedDatabaseItem{}, fmt.Errorf("RSS Feed database entry does not have any Title in 'Title' field")
    }

    return &FeedDatabaseItem{
        FeedLink:     rssUrl,
        Created:      p.CreatedTime,
        LastModified: p.LastEditedTime,
        Name:         nameRichTexts[0].PlainText,
    }, nil
}

func GetImageUrl(x string) *string {
    // Extract the first image src from the document to use as cover
    re := regexp.MustCompile(`(?m)<img\b[^>]+?src\s*=\s*['"]?([^\s'"?#>]+)`)
    match := re.FindSubmatch([]byte(x))
    if match != nil {
        v := string(match[1])
        if strings.HasPrefix(v, "http") {
            return &v
        } else {
            fmt.Printf("[ERROR]: Invalid image url found in <img> url=%s\n", string(match[1]))
            return nil
        }
    }
    return nil
}

// ProcessCategories processes the input string and returns a slice of categories.
func ProcessCategories(input string) []string {
    // Split the input string by comma to get individual categories.
    rawCategories := strings.Split(input, ",")

    // Initialize a slice to hold the processed categories.
    var categories []string

    // Loop through the raw categories and process each one.
    for _, rawCategory := range rawCategories {
        // Remove the prefix (e.g., "general:").
        parts := strings.SplitN(rawCategory, ":", 2)
        if len(parts) == 2 {
            categories = append(categories, parts[1])
        }
    }

    return categories
}

// AddRssItem to Notion database as a single new page with Block content. On failure, no retry is attempted.
func (dao NotionDao) AddRssItem(item RssItem) error {
    // Check for duplicate entries based on title and link
    queryRequest := &notionapi.DatabaseQueryRequest{
        Filter: notionapi.AndCompoundFilter([]notionapi.Filter{
            notionapi.PropertyFilter{
                Property: "Title",
                Text: &notionapi.TextFilterCondition{
                    Equals: item.title,
                },
            },
            notionapi.PropertyFilter{
                Property: "Link",
                Text: &notionapi.TextFilterCondition{
                    Equals: item.link.String(),
                },
            },
        }),
    }
    resp, err := dao.client.Database.Query(context.Background(), dao.contentDatabaseId, queryRequest)
    if err != nil {
        return fmt.Errorf("failed to query database for duplicates: %v", err)
    }
    if len(resp.Results) > 0 {
        return fmt.Errorf("duplicate item found with title: %s and link: %s", item.title, item.link.String())
    }
    
    // Process the categories input string
    item.categories = ProcessCategories(strings.Join(item.categories, ","))

    categories := make([]notionapi.Option, len(item.categories))
    for i, c := range item.categories {
        categories[i] = notionapi.Option{
            Name: c,
        }
    }
    var imageProp *notionapi.Image
    image := GetImageUrl(strings.Join(item.content, " "))
    if image != nil {
        imageProp = &notionapi.Image{
            Type: "external",
            External: &notionapi.FileObject{
                URL: *image,
            },
        }
    }

    descriptionChunks := chunkString(*item.description, 2000)
    richTextChunks := make([]notionapi.RichText, len(descriptionChunks))
    for i, chunk := range descriptionChunks {
        richTextChunks[i] = notionapi.RichText{
            Type: notionapi.ObjectTypeText,
            Text: notionapi.Text{
                Content: chunk,
            },
            PlainText: chunk,
        }
    }

    _, err = dao.client.Page.Create(context.Background(), &notionapi.PageCreateRequest{
        Parent: notionapi.Parent{
            Type:       "database_id",
            DatabaseID: dao.contentDatabaseId,
        },
        Properties: map[string]notionapi.Property{
            "Title": notionapi.TitleProperty{
                Type: "title",
                Title: []notionapi.RichText{{
                    Type: "text",
                    Text: notionapi.Text{
                        Content: item.title,
                    },
                }},
            },
            "Description": notionapi.RichTextProperty{
                Type:     "rich_text",
                RichText: richTextChunks,
            },
            "Link": notionapi.URLProperty{
                Type: "url",
                URL:  item.link.String(),
            },
            "Categories": notionapi.MultiSelectProperty{
                MultiSelect: categories,
            },
            "From":      notionapi.SelectProperty{Select: notionapi.Option{Name: item.feedName}},
            "Published": notionapi.DateProperty{Date: &notionapi.DateObject{Start: (*notionapi.Date)(item.published)}},
        },
        Children: RssContentToBlocks(item),
        Cover:    imageProp,
    })
    return err
}

func chunkString(s string, chunkSize int) []string {
    var chunks []string
    for i := 0; i < len(s); i += chunkSize {
        end := i + chunkSize
        if end > len(s) {
            end = len(s)
        }
        chunks = append(chunks, s[i:end])
    }
    return chunks
}

func RssContentToBlocks(item RssItem) []notionapi.Block {
    // TODO: implement when we know RssItem struct better
    return []notionapi.Block{}
}
