import { API_SAME_ORIGIN, SIGN_OUT_PATH } from '@/api/signin';
import { ThemeSelector } from '@/shell/ThemeSelector';
import { ImportProjectSection } from './ImportProjectSection';
import { SessionDefaultsSection } from './SessionDefaultsSection';

/** Settings: what the owner sets once and Mainloop then applies everywhere. */
export function SettingsPage() {
  return (
    <div className="gap-lg p-md mx-auto flex w-full max-w-3xl flex-col" data-testid="settings-page">
      <h1 className="text-lg font-semibold">Settings</h1>
      <SessionDefaultsSection />
      <ImportProjectSection />
      <section aria-labelledby="appearance-heading" className="gap-sm flex flex-col">
        <h2 id="appearance-heading" className="text-base font-semibold">
          Appearance
        </h2>
        <div className="gap-sm flex flex-wrap items-center">
          <span className="text-muted-foreground text-sm">Theme, kept in this browser.</span>
          <ThemeSelector />
        </div>
      </section>
      {API_SAME_ORIGIN ? (
        <section aria-labelledby="account-heading" className="gap-sm flex flex-col">
          <h2 id="account-heading" className="text-base font-semibold">
            Account
          </h2>
          <div className="gap-sm flex flex-wrap items-center">
            <span className="text-muted-foreground text-sm">
              Signed in with Pocket ID. Signing out ends it in this browser.
            </span>
            {/* A full navigation: the gateway serves this path, not the app's router. */}
            <a
              href={SIGN_OUT_PATH}
              className="px-sm border-border inline-flex min-h-11 items-center border text-sm font-medium hover:underline"
              data-testid="sign-out"
            >
              Sign out
            </a>
          </div>
        </section>
      ) : null}
    </div>
  );
}
