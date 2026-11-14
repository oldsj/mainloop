import { ThemeSelector } from '@/shell/ThemeSelector';
import { SessionDefaultsSection } from './SessionDefaultsSection';

/** Settings: what the owner sets once and Mainloop then applies everywhere. */
export function SettingsPage() {
  return (
    <div className="gap-lg p-md mx-auto flex w-full max-w-3xl flex-col" data-testid="settings-page">
      <h1 className="text-lg font-semibold">Settings</h1>
      <SessionDefaultsSection />
      <section aria-labelledby="appearance-heading" className="gap-sm flex flex-col">
        <h2 id="appearance-heading" className="text-base font-semibold">
          Appearance
        </h2>
        <div className="gap-sm flex flex-wrap items-center">
          <span className="text-muted-foreground text-sm">Theme, kept in this browser.</span>
          <ThemeSelector />
        </div>
      </section>
    </div>
  );
}
