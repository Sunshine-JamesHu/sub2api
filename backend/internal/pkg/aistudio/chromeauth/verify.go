package chromeauth

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

// Verify the issued web session instead of trusting only cached profile metadata.
func verifyImportedAccount(ctx context.Context, state *aistudio.StorageState, proxy, email string) error {
	session, err := aistudio.NewSession(aistudio.SessionOptions{State: *state, ProxyURL: proxy})
	if err != nil {
		return err
	}
	defer session.Close()
	if err := session.VerifyAccountEmail(ctx, email); err != nil {
		return fmt.Errorf("验证 Chrome 导入账号: %w", err)
	}
	*state = session.StorageState()
	return nil
}
