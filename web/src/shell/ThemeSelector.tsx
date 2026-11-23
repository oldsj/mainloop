import { Check, Palette } from 'lucide-react';
import { DropdownMenu } from 'radix-ui';
import { Button } from '@/components/ui/button';
import { THEMES, useTheme } from './themes';

/**
 * The theme menu. `compact` is the activity bar's form: the icon alone, with the menu opening to the
 * side of the bar.
 */
export function ThemeSelector({ compact = false }: { compact?: boolean }) {
  const [theme, setTheme] = useTheme();
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button
          variant="ghost"
          size={compact ? 'icon' : 'default'}
          data-testid="theme-selector"
          aria-label={`Theme: ${theme}`}
          title={compact ? `Theme: ${theme}` : undefined}
          className={compact ? 'text-muted-foreground hover:text-foreground size-9' : undefined}
        >
          <Palette aria-hidden="true" />
          {compact ? null : <span className="hidden sm:inline">{theme}</span>}
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          side={compact ? 'right' : 'bottom'}
          align="end"
          sideOffset={4}
          className="border-border bg-popover p-xs text-popover-foreground z-50 min-w-44 rounded-none border"
        >
          <DropdownMenu.RadioGroup
            value={theme}
            onValueChange={(value) => {
              const next = THEMES.find((candidate) => candidate.id === value);
              if (next) setTheme(next.id);
            }}
          >
            {THEMES.map(({ id, label }) => (
              // Each row previews its own palette: theme.css scopes the variables to [data-theme].
              <DropdownMenu.RadioItem
                key={id}
                value={id}
                data-testid={`theme-${id}`}
                className="gap-sm px-sm data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground min-h-control flex cursor-pointer items-center rounded-none text-sm outline-none data-[highlighted]:font-bold"
              >
                <span
                  data-theme={id === 'eighties' ? undefined : id}
                  className="border-border flex h-3 w-6 shrink-0 border"
                  aria-hidden="true"
                >
                  <span className="bg-term-bg flex-1" />
                  <span className="bg-term-accent flex-1" />
                  <span className="bg-term-green flex-1" />
                </span>
                <span className="flex-1">{label}</span>
                <DropdownMenu.ItemIndicator>
                  <Check className="size-4" aria-hidden="true" />
                </DropdownMenu.ItemIndicator>
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
