import { cva, type VariantProps } from 'class-variance-authority';
import { Slot } from 'radix-ui';
import type { ComponentProps } from 'react';
import { cn } from './cn';

const buttonVariants = cva(
  'inline-flex min-h-11 shrink-0 items-center justify-center gap-sm whitespace-nowrap rounded-none border border-transparent px-md text-sm font-medium transition-colors disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground hover:bg-primary/90',
        destructive: 'bg-destructive text-destructive-foreground hover:bg-destructive/90',
        outline: 'border-border bg-transparent text-foreground hover:bg-accent',
        ghost: 'text-foreground hover:bg-accent'
      },
      size: { default: '', sm: 'px-sm text-xs', icon: 'w-11 px-0' }
    },
    defaultVariants: { variant: 'default', size: 'default' }
  }
);

export function Button({
  className,
  variant,
  size,
  asChild = false,
  type = 'button',
  ...props
}: ComponentProps<'button'> & VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : 'button';
  return (
    <Comp
      type={asChild ? undefined : type}
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  );
}

export { buttonVariants };
