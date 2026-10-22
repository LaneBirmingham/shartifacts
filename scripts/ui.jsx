// shadcn/ui-compatible components for React artifacts: imports such as
// `@/components/ui/card` resolve here. Same names, props and Tailwind classes as shadcn,
// without Radix: enough behaviour for small tools, not a full accessibility layer.
import React, {
  cloneElement,
  createContext,
  forwardRef,
  isValidElement,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

export function cn(...inputs) {
  const out = [];
  const add = (v) => {
    if (!v) return;
    if (typeof v === "string" || typeof v === "number") out.push(String(v));
    else if (Array.isArray(v)) v.forEach(add);
    else if (typeof v === "object")
      for (const [k, on] of Object.entries(v)) if (on) out.push(k);
  };
  inputs.forEach(add);
  return out.join(" ");
}

// Controlled-or-uncontrolled state, as Radix primitives behave.
function useControllable(value, defaultValue, onChange) {
  const [inner, setInner] = useState(defaultValue);
  const controlled = value !== undefined;
  const current = controlled ? value : inner;
  const set = (next) => {
    if (!controlled) setInner(next);
    onChange?.(next);
  };
  return [current, set];
}

// asChild: merge props into the single child instead of rendering a wrapper element.
const Slot = forwardRef(({ children, ...props }, ref) => {
  if (!isValidElement(children)) return null;
  const merged = { ...props, ...children.props, ref };
  for (const key of Object.keys(props))
    if (/^on[A-Z]/.test(key) && typeof children.props[key] === "function")
      merged[key] = (...a) => {
        children.props[key](...a);
        props[key](...a);
      };
  merged.className = cn(props.className, children.props.className);
  return cloneElement(children, merged);
});
const Element = (asChild, tag) => (asChild ? Slot : tag);

function simple(tag, base, name) {
  const C = forwardRef(({ className, asChild, ...props }, ref) => {
    const T = Element(asChild, tag);
    return <T ref={ref} className={cn(base, className)} {...props} />;
  });
  C.displayName = name;
  return C;
}

// Button
const buttonVariants = ({ variant = "default", size = "default", className } = {}) =>
  cn(
    "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0 cursor-pointer",
    {
      default: "bg-primary text-primary-foreground hover:bg-primary/90",
      destructive: "bg-destructive text-destructive-foreground hover:bg-destructive/90",
      outline: "border border-input bg-background hover:bg-accent hover:text-accent-foreground",
      secondary: "bg-secondary text-secondary-foreground hover:bg-secondary/80",
      ghost: "hover:bg-accent hover:text-accent-foreground",
      link: "text-primary underline-offset-4 hover:underline",
    }[variant],
    { default: "h-10 px-4 py-2", sm: "h-9 rounded-md px-3", lg: "h-11 rounded-md px-8", icon: "h-10 w-10" }[size],
    className,
  );
export { buttonVariants };
export const Button = forwardRef(({ className, variant, size, asChild, ...props }, ref) => {
  const T = Element(asChild, "button");
  return <T ref={ref} className={buttonVariants({ variant, size, className })} {...props} />;
});

// Card
export const Card = simple("div", "rounded-lg border bg-card text-card-foreground shadow-sm", "Card");
export const CardHeader = simple("div", "flex flex-col space-y-1.5 p-6", "CardHeader");
export const CardTitle = simple("h3", "text-2xl font-semibold leading-none tracking-tight", "CardTitle");
export const CardDescription = simple("p", "text-sm text-muted-foreground", "CardDescription");
export const CardContent = simple("div", "p-6 pt-0", "CardContent");
export const CardFooter = simple("div", "flex items-center p-6 pt-0", "CardFooter");

// Alert
export const Alert = forwardRef(({ className, variant = "default", ...props }, ref) => (
  <div
    ref={ref}
    role="alert"
    className={cn(
      "relative w-full rounded-lg border p-4 [&>svg~*]:pl-7 [&>svg+div]:translate-y-[-3px] [&>svg]:absolute [&>svg]:left-4 [&>svg]:top-4 [&>svg]:text-foreground",
      variant === "destructive"
        ? "border-destructive/50 text-destructive [&>svg]:text-destructive"
        : "bg-background text-foreground",
      className,
    )}
    {...props}
  />
));
export const AlertTitle = simple("h5", "mb-1 font-medium leading-none tracking-tight", "AlertTitle");
export const AlertDescription = simple("div", "text-sm [&_p]:leading-relaxed", "AlertDescription");

// Badge
export const badgeVariants = ({ variant = "default", className } = {}) =>
  cn(
    "inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-semibold transition-colors",
    {
      default: "border-transparent bg-primary text-primary-foreground hover:bg-primary/80",
      secondary: "border-transparent bg-secondary text-secondary-foreground hover:bg-secondary/80",
      destructive: "border-transparent bg-destructive text-destructive-foreground hover:bg-destructive/80",
      outline: "text-foreground",
    }[variant],
    className,
  );
export const Badge = ({ className, variant, ...props }) => (
  <div className={badgeVariants({ variant, className })} {...props} />
);

// Form controls
export const Input = forwardRef(({ className, type, ...props }, ref) => (
  <input
    type={type}
    ref={ref}
    className={cn(
      "flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background file:border-0 file:bg-transparent file:text-sm file:font-medium placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
      className,
    )}
    {...props}
  />
));
export const Textarea = forwardRef(({ className, ...props }, ref) => (
  <textarea
    ref={ref}
    className={cn(
      "flex min-h-[80px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
      className,
    )}
    {...props}
  />
));
export const Label = simple(
  "label",
  "text-sm font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70",
  "Label",
);

export const Checkbox = forwardRef(
  ({ className, checked, defaultChecked, onCheckedChange, disabled, ...props }, ref) => {
    const [on, set] = useControllable(checked, defaultChecked ?? false, onCheckedChange);
    return (
      <button
        ref={ref}
        type="button"
        role="checkbox"
        aria-checked={on}
        data-state={on ? "checked" : "unchecked"}
        disabled={disabled}
        onClick={() => set(!on)}
        className={cn(
          "peer inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-sm border border-primary ring-offset-background focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
          on && "bg-primary text-primary-foreground",
          className,
        )}
        {...props}
      >
        {on && (
          <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="3">
            <path d="M20 6 9 17l-5-5" />
          </svg>
        )}
      </button>
    );
  },
);

export const Switch = forwardRef(
  ({ className, checked, defaultChecked, onCheckedChange, disabled, ...props }, ref) => {
    const [on, set] = useControllable(checked, defaultChecked ?? false, onCheckedChange);
    return (
      <button
        ref={ref}
        type="button"
        role="switch"
        aria-checked={on}
        data-state={on ? "checked" : "unchecked"}
        disabled={disabled}
        onClick={() => set(!on)}
        className={cn(
          "peer inline-flex h-6 w-11 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
          on ? "bg-primary" : "bg-input",
          className,
        )}
        {...props}
      >
        <span
          className={cn(
            "pointer-events-none block h-5 w-5 rounded-full bg-background shadow-lg ring-0 transition-transform",
            on ? "translate-x-5" : "translate-x-0",
          )}
        />
      </button>
    );
  },
);

export const Slider = forwardRef(
  ({ className, value, defaultValue, onValueChange, onValueCommit, min = 0, max = 100, step = 1, disabled, ...props }, ref) => {
    const [v, set] = useControllable(value, defaultValue ?? [min], onValueChange);
    const pct = ((v[0] - min) / (max - min || 1)) * 100;
    return (
      <span ref={ref} className={cn("relative flex w-full touch-none select-none items-center", className)} {...props}>
        <span className="relative h-2 w-full grow overflow-hidden rounded-full bg-secondary">
          <span className="absolute h-full bg-primary" style={{ width: pct + "%" }} />
        </span>
        <span
          className="pointer-events-none absolute block h-5 w-5 -translate-x-1/2 rounded-full border-2 border-primary bg-background ring-offset-background transition-colors"
          style={{ left: pct + "%" }}
        />
        <input
          type="range"
          aria-label={props["aria-label"]}
          min={min}
          max={max}
          step={step}
          value={v[0]}
          disabled={disabled}
          onChange={(e) => set([Number(e.target.value)])}
          onPointerUp={() => onValueCommit?.(v)}
          className="absolute inset-0 w-full cursor-pointer opacity-0"
        />
      </span>
    );
  },
);

export const Progress = forwardRef(({ className, value = 0, ...props }, ref) => (
  <div
    ref={ref}
    role="progressbar"
    aria-valuenow={value}
    className={cn("relative h-4 w-full overflow-hidden rounded-full bg-secondary", className)}
    {...props}
  >
    <div className="h-full w-full flex-1 bg-primary transition-all" style={{ transform: `translateX(-${100 - (value || 0)}%)` }} />
  </div>
));

export const Separator = forwardRef(({ className, orientation = "horizontal", decorative = true, ...props }, ref) => (
  <div
    ref={ref}
    role={decorative ? "none" : "separator"}
    className={cn("shrink-0 bg-border", orientation === "horizontal" ? "h-[1px] w-full" : "h-full w-[1px]", className)}
    {...props}
  />
));
export const Skeleton = ({ className, ...props }) => (
  <div className={cn("animate-pulse rounded-md bg-muted", className)} {...props} />
);
export const ScrollArea = forwardRef(({ className, children, ...props }, ref) => (
  <div ref={ref} className={cn("relative overflow-auto", className)} {...props}>
    {children}
  </div>
));
export const ScrollBar = () => null;
export const AspectRatio = ({ ratio = 1, style, ...props }) => (
  <div style={{ aspectRatio: String(ratio), ...style }} {...props} />
);

// Radio group
const RadioContext = createContext(null);
export const RadioGroup = forwardRef(({ className, value, defaultValue, onValueChange, name, ...props }, ref) => {
  const [v, set] = useControllable(value, defaultValue, onValueChange);
  const id = useId();
  return (
    <RadioContext.Provider value={{ value: v, set, name: name ?? id }}>
      <div ref={ref} role="radiogroup" className={cn("grid gap-2", className)} {...props} />
    </RadioContext.Provider>
  );
});
export const RadioGroupItem = forwardRef(({ className, value, id, disabled, ...props }, ref) => {
  const ctx = useContext(RadioContext);
  const on = ctx?.value === value;
  return (
    <button
      ref={ref}
      id={id}
      type="button"
      role="radio"
      aria-checked={on}
      data-state={on ? "checked" : "unchecked"}
      disabled={disabled}
      onClick={() => ctx?.set(value)}
      className={cn(
        "aspect-square h-4 w-4 rounded-full border border-primary text-primary ring-offset-background focus:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 inline-flex items-center justify-center",
        className,
      )}
      {...props}
    >
      {on && <span className="h-2.5 w-2.5 rounded-full bg-current" />}
    </button>
  );
});

// Tabs
const TabsContext = createContext(null);
export const Tabs = forwardRef(({ value, defaultValue, onValueChange, className, ...props }, ref) => {
  const [v, set] = useControllable(value, defaultValue, onValueChange);
  return (
    <TabsContext.Provider value={{ value: v, set }}>
      <div ref={ref} className={className} {...props} />
    </TabsContext.Provider>
  );
});
export const TabsList = simple(
  "div",
  "inline-flex h-10 items-center justify-center rounded-md bg-muted p-1 text-muted-foreground",
  "TabsList",
);
export const TabsTrigger = forwardRef(({ className, value, disabled, ...props }, ref) => {
  const ctx = useContext(TabsContext);
  const on = ctx?.value === value;
  return (
    <button
      ref={ref}
      type="button"
      role="tab"
      aria-selected={on}
      data-state={on ? "active" : "inactive"}
      disabled={disabled}
      onClick={() => ctx?.set(value)}
      className={cn(
        "inline-flex items-center justify-center whitespace-nowrap rounded-sm px-3 py-1.5 text-sm font-medium ring-offset-background transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50 cursor-pointer",
        on && "bg-background text-foreground shadow-sm",
        className,
      )}
      {...props}
    />
  );
});
export const TabsContent = forwardRef(({ className, value, ...props }, ref) => {
  const ctx = useContext(TabsContext);
  if (ctx?.value !== value) return null;
  return (
    <div
      ref={ref}
      role="tabpanel"
      data-state="active"
      className={cn("mt-2 ring-offset-background focus-visible:outline-none", className)}
      {...props}
    />
  );
});

// Accordion and collapsible
const AccordionContext = createContext(null);
const ItemContext = createContext(null);
export const Accordion = forwardRef(
  ({ type = "single", collapsible, value, defaultValue, onValueChange, className, ...props }, ref) => {
    const [v, set] = useControllable(value, defaultValue ?? (type === "multiple" ? [] : ""), onValueChange);
    const isOpen = (item) => (type === "multiple" ? (v || []).includes(item) : v === item);
    const toggle = (item) => {
      if (type === "multiple") set(isOpen(item) ? v.filter((x) => x !== item) : [...(v || []), item]);
      else set(isOpen(item) ? (collapsible ? "" : item) : item);
    };
    return (
      <AccordionContext.Provider value={{ isOpen, toggle }}>
        <div ref={ref} className={className} {...props} />
      </AccordionContext.Provider>
    );
  },
);
export const AccordionItem = forwardRef(({ value, className, ...props }, ref) => {
  const ctx = useContext(AccordionContext);
  const open = ctx?.isOpen(value);
  return (
    <ItemContext.Provider value={{ open, toggle: () => ctx?.toggle(value) }}>
      <div ref={ref} data-state={open ? "open" : "closed"} className={cn("border-b", className)} {...props} />
    </ItemContext.Provider>
  );
});
const Chevron = ({ open }) => (
  <svg
    viewBox="0 0 24 24"
    width="16"
    height="16"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    className="shrink-0 transition-transform duration-200"
    style={{ transform: open ? "rotate(180deg)" : undefined }}
  >
    <path d="m6 9 6 6 6-6" />
  </svg>
);
export const AccordionTrigger = forwardRef(({ className, children, ...props }, ref) => {
  const item = useContext(ItemContext);
  return (
    <h3 className="flex">
      <button
        ref={ref}
        type="button"
        aria-expanded={!!item?.open}
        data-state={item?.open ? "open" : "closed"}
        onClick={item?.toggle}
        className={cn(
          "flex flex-1 items-center justify-between py-4 font-medium transition-all hover:underline cursor-pointer",
          className,
        )}
        {...props}
      >
        {children}
        <Chevron open={item?.open} />
      </button>
    </h3>
  );
});
export const AccordionContent = forwardRef(({ className, children, ...props }, ref) => {
  const item = useContext(ItemContext);
  if (!item?.open) return null;
  return (
    <div ref={ref} data-state="open" className="overflow-hidden text-sm" {...props}>
      <div className={cn("pb-4 pt-0", className)}>{children}</div>
    </div>
  );
});
const CollapsibleContext = createContext(null);
export const Collapsible = ({ open, defaultOpen, onOpenChange, children, className, ...props }) => {
  const [o, set] = useControllable(open, defaultOpen ?? false, onOpenChange);
  return (
    <CollapsibleContext.Provider value={{ open: o, set }}>
      <div data-state={o ? "open" : "closed"} className={className} {...props}>
        {children}
      </div>
    </CollapsibleContext.Provider>
  );
};
export const CollapsibleTrigger = forwardRef(({ asChild, onClick, ...props }, ref) => {
  const ctx = useContext(CollapsibleContext);
  const T = Element(asChild, "button");
  return (
    <T
      ref={ref}
      type={asChild ? undefined : "button"}
      aria-expanded={!!ctx?.open}
      onClick={(e) => {
        onClick?.(e);
        ctx?.set(!ctx.open);
      }}
      {...props}
    />
  );
});
export const CollapsibleContent = ({ children, ...props }) =>
  useContext(CollapsibleContext)?.open ? <div {...props}>{children}</div> : null;

// Overlays: dialog, alert dialog, popover, tooltip, dropdown menu, select
function useOverlay(open, onOpenChange, defaultOpen) {
  const [o, set] = useControllable(open, defaultOpen ?? false, onOpenChange);
  return { open: o, setOpen: set };
}
const DialogContext = createContext(null);
export const Dialog = ({ open, defaultOpen, onOpenChange, children }) => (
  <DialogContext.Provider value={useOverlay(open, onOpenChange, defaultOpen)}>{children}</DialogContext.Provider>
);
export const AlertDialog = Dialog;
function Trigger(Context, name) {
  const C = forwardRef(({ asChild, onClick, ...props }, ref) => {
    const ctx = useContext(Context);
    const T = Element(asChild, "button");
    return (
      <T
        ref={ref}
        type={asChild ? undefined : "button"}
        data-state={ctx?.open ? "open" : "closed"}
        onClick={(e) => {
          onClick?.(e);
          ctx?.setOpen(!ctx.open);
        }}
        {...props}
      />
    );
  });
  C.displayName = name;
  return C;
}
export const DialogTrigger = Trigger(DialogContext, "DialogTrigger");
export const AlertDialogTrigger = DialogTrigger;
const Close = forwardRef(({ asChild, onClick, ...props }, ref) => {
  const ctx = useContext(DialogContext);
  const T = Element(asChild, "button");
  return (
    <T
      ref={ref}
      type={asChild ? undefined : "button"}
      onClick={(e) => {
        onClick?.(e);
        ctx?.setOpen(false);
      }}
      {...props}
    />
  );
});
export const DialogClose = Close;
export const DialogPortal = ({ children }) => children;
export const DialogOverlay = () => null;
export const DialogContent = forwardRef(({ className, children, ...props }, ref) => {
  const ctx = useContext(DialogContext);
  useEffect(() => {
    if (!ctx?.open) return;
    const key = (e) => e.key === "Escape" && ctx.setOpen(false);
    addEventListener("keydown", key);
    return () => removeEventListener("keydown", key);
  }, [ctx?.open]);
  if (!ctx?.open) return null;
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="fixed inset-0 bg-black/80" onClick={() => ctx.setOpen(false)} />
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        className={cn(
          "relative z-50 grid w-full max-w-lg gap-4 border bg-background p-6 shadow-lg sm:rounded-lg",
          className,
        )}
        {...props}
      >
        {children}
        <button
          type="button"
          aria-label="Close"
          onClick={() => ctx.setOpen(false)}
          className="absolute right-4 top-4 rounded-sm opacity-70 transition-opacity hover:opacity-100 cursor-pointer"
        >
          ✕
        </button>
      </div>
    </div>,
    document.body,
  );
});
export const AlertDialogContent = forwardRef(({ className, children, ...props }, ref) => {
  const ctx = useContext(DialogContext);
  if (!ctx?.open) return null;
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="fixed inset-0 bg-black/80" />
      <div
        ref={ref}
        role="alertdialog"
        aria-modal="true"
        className={cn("relative z-50 grid w-full max-w-lg gap-4 border bg-background p-6 shadow-lg sm:rounded-lg", className)}
        {...props}
      >
        {children}
      </div>
    </div>,
    document.body,
  );
});
export const DialogHeader = simple("div", "flex flex-col space-y-1.5 text-center sm:text-left", "DialogHeader");
export const DialogFooter = simple(
  "div",
  "flex flex-col-reverse sm:flex-row sm:justify-end sm:space-x-2",
  "DialogFooter",
);
export const DialogTitle = simple("h2", "text-lg font-semibold leading-none tracking-tight", "DialogTitle");
export const DialogDescription = simple("p", "text-sm text-muted-foreground", "DialogDescription");
export const AlertDialogHeader = DialogHeader;
export const AlertDialogFooter = DialogFooter;
export const AlertDialogTitle = DialogTitle;
export const AlertDialogDescription = DialogDescription;
export const AlertDialogAction = forwardRef(({ className, ...props }, ref) => (
  <Close ref={ref} className={buttonVariants({ className })} {...props} />
));
export const AlertDialogCancel = forwardRef(({ className, ...props }, ref) => (
  <Close ref={ref} className={buttonVariants({ variant: "outline", className: cn("mt-2 sm:mt-0", className) })} {...props} />
));
export const Sheet = Dialog;
export const SheetTrigger = DialogTrigger;
export const SheetClose = DialogClose;
export const SheetContent = forwardRef(({ className, side = "right", children, ...props }, ref) => {
  const ctx = useContext(DialogContext);
  if (!ctx?.open) return null;
  const sides = {
    right: "inset-y-0 right-0 h-full w-3/4 border-l sm:max-w-sm",
    left: "inset-y-0 left-0 h-full w-3/4 border-r sm:max-w-sm",
    top: "inset-x-0 top-0 border-b",
    bottom: "inset-x-0 bottom-0 border-t",
  };
  return createPortal(
    <div className="fixed inset-0 z-50">
      <div className="fixed inset-0 bg-black/80" onClick={() => ctx.setOpen(false)} />
      <div ref={ref} role="dialog" className={cn("fixed z-50 gap-4 bg-background p-6 shadow-lg", sides[side], className)} {...props}>
        {children}
      </div>
    </div>,
    document.body,
  );
});
export const SheetHeader = DialogHeader;
export const SheetFooter = DialogFooter;
export const SheetTitle = DialogTitle;
export const SheetDescription = DialogDescription;

// Anchored floating content shared by popover, tooltip, dropdown and select.
const FloatContext = createContext(null);
function FloatRoot({ open, defaultOpen, onOpenChange, children, hover }) {
  const state = useOverlay(open, onOpenChange, defaultOpen);
  const anchor = useRef(null);
  return (
    <FloatContext.Provider value={{ ...state, anchor, hover }}>
      <span className="relative inline-block">{children}</span>
    </FloatContext.Provider>
  );
}
function useDismiss(ctx, ref) {
  useEffect(() => {
    if (!ctx?.open || ctx.hover) return;
    const down = (e) => {
      if (!ref.current?.contains(e.target) && !ctx.anchor.current?.contains(e.target)) ctx.setOpen(false);
    };
    const key = (e) => e.key === "Escape" && ctx.setOpen(false);
    document.addEventListener("pointerdown", down);
    addEventListener("keydown", key);
    return () => {
      document.removeEventListener("pointerdown", down);
      removeEventListener("keydown", key);
    };
  }, [ctx?.open]);
}
const FloatTrigger = forwardRef(({ asChild, onClick, ...props }, ref) => {
  const ctx = useContext(FloatContext);
  const T = Element(asChild, "button");
  const events = ctx?.hover
    ? { onMouseEnter: () => ctx.setOpen(true), onMouseLeave: () => ctx.setOpen(false), onFocus: () => ctx.setOpen(true), onBlur: () => ctx.setOpen(false) }
    : {
        onClick: (e) => {
          onClick?.(e);
          ctx?.setOpen(!ctx.open);
        },
      };
  return (
    <T
      ref={(el) => {
        if (ctx) ctx.anchor.current = el;
        if (typeof ref === "function") ref(el);
        else if (ref) ref.current = el;
      }}
      type={asChild ? undefined : "button"}
      aria-expanded={!!ctx?.open}
      data-state={ctx?.open ? "open" : "closed"}
      {...events}
      {...props}
    />
  );
});
const FloatContent = forwardRef(({ className, align = "center", side = "bottom", sideOffset = 4, children, style, ...props }, _ref) => {
  const ctx = useContext(FloatContext);
  const ref = useRef(null);
  useDismiss(ctx, ref);
  if (!ctx?.open) return null;
  const position =
    side === "top"
      ? { bottom: `calc(100% + ${sideOffset}px)` }
      : { top: `calc(100% + ${sideOffset}px)` };
  const x = align === "start" ? { left: 0 } : align === "end" ? { right: 0 } : { left: "50%", transform: "translateX(-50%)" };
  return (
    <div ref={ref} className={cn("absolute z-50", className)} style={{ ...position, ...x, ...style }} {...props}>
      {children}
    </div>
  );
});
export const Popover = (p) => <FloatRoot {...p} />;
export const PopoverTrigger = FloatTrigger;
export const PopoverContent = forwardRef(({ className, ...props }, ref) => (
  <FloatContent
    ref={ref}
    className={cn("w-72 rounded-md border bg-popover p-4 text-popover-foreground shadow-md outline-none", className)}
    {...props}
  />
));
export const TooltipProvider = ({ children }) => children;
export const Tooltip = (p) => <FloatRoot {...p} hover />;
export const TooltipTrigger = FloatTrigger;
export const TooltipContent = forwardRef(({ className, side = "top", ...props }, ref) => (
  <FloatContent
    ref={ref}
    side={side}
    className={cn(
      "overflow-hidden whitespace-nowrap rounded-md border bg-popover px-3 py-1.5 text-sm text-popover-foreground shadow-md",
      className,
    )}
    {...props}
  />
));
export const DropdownMenu = (p) => <FloatRoot {...p} />;
export const DropdownMenuTrigger = FloatTrigger;
export const DropdownMenuContent = forwardRef(({ className, align = "start", ...props }, ref) => (
  <FloatContent
    ref={ref}
    align={align}
    role="menu"
    className={cn("min-w-[8rem] overflow-hidden rounded-md border bg-popover p-1 text-popover-foreground shadow-md", className)}
    {...props}
  />
));
export const DropdownMenuItem = forwardRef(({ className, onClick, onSelect, inset, disabled, ...props }, ref) => {
  const ctx = useContext(FloatContext);
  return (
    <div
      ref={ref}
      role="menuitem"
      tabIndex={-1}
      aria-disabled={disabled}
      onClick={(e) => {
        if (disabled) return;
        onClick?.(e);
        onSelect?.(e);
        ctx?.setOpen(false);
      }}
      className={cn(
        "relative flex cursor-pointer select-none items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none transition-colors hover:bg-accent hover:text-accent-foreground",
        inset && "pl-8",
        disabled && "pointer-events-none opacity-50",
        className,
      )}
      {...props}
    />
  );
});
export const DropdownMenuLabel = simple("div", "px-2 py-1.5 text-sm font-semibold", "DropdownMenuLabel");
export const DropdownMenuSeparator = simple("div", "-mx-1 my-1 h-px bg-muted", "DropdownMenuSeparator");
export const DropdownMenuGroup = simple("div", "", "DropdownMenuGroup");

// Select: Radix's compound API over a floating list.
const SelectContext = createContext(null);
export const Select = ({ value, defaultValue, onValueChange, open, defaultOpen, onOpenChange, disabled, children }) => {
  const [v, set] = useControllable(value, defaultValue, onValueChange);
  const labels = useRef({});
  const [, rerender] = useState(0);
  return (
    <SelectContext.Provider value={{ value: v, set, labels, disabled, rerender }}>
      <FloatRoot open={open} defaultOpen={defaultOpen} onOpenChange={onOpenChange}>
        {children}
      </FloatRoot>
    </SelectContext.Provider>
  );
};
export const SelectTrigger = forwardRef(({ className, children, ...props }, ref) => {
  const sel = useContext(SelectContext);
  return (
    <FloatTrigger
      ref={ref}
      role="combobox"
      disabled={sel?.disabled}
      className={cn(
        "flex h-10 w-full min-w-[8rem] items-center justify-between gap-2 rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 cursor-pointer",
        className,
      )}
      {...props}
    >
      {children}
      <Chevron />
    </FloatTrigger>
  );
});
export const SelectValue = ({ placeholder }) => {
  const sel = useContext(SelectContext);
  const label = sel?.value !== undefined && sel?.value !== "" ? sel.labels.current[sel.value] ?? sel.value : undefined;
  return <span className={cn("truncate", label === undefined && "text-muted-foreground")}>{label ?? placeholder}</span>;
};
export const SelectContent = forwardRef(({ className, children, position, ...props }, ref) => {
  const ctx = useContext(FloatContext);
  return (
    <>
      {/* Items stay mounted (hidden) so SelectValue can show the chosen label. */}
      <div hidden>{!ctx?.open && children}</div>
      <FloatContent
        ref={ref}
        align="start"
        role="listbox"
        className={cn(
          "max-h-96 min-w-[8rem] w-full overflow-auto rounded-md border bg-popover p-1 text-popover-foreground shadow-md",
          className,
        )}
        {...props}
      >
        {children}
      </FloatContent>
    </>
  );
});
export const SelectItem = forwardRef(({ className, value, children, disabled, ...props }, ref) => {
  const sel = useContext(SelectContext);
  const float = useContext(FloatContext);
  if (sel && typeof children === "string" && sel.labels.current[value] !== children) {
    sel.labels.current[value] = children;
    queueMicrotask(() => sel.rerender((n) => n + 1));
  }
  const on = sel?.value === value;
  return (
    <div
      ref={ref}
      role="option"
      aria-selected={on}
      aria-disabled={disabled}
      onClick={() => {
        if (disabled) return;
        sel?.set(value);
        float?.setOpen(false);
      }}
      className={cn(
        "relative flex w-full cursor-pointer select-none items-center rounded-sm py-1.5 pl-8 pr-2 text-sm outline-none hover:bg-accent hover:text-accent-foreground",
        disabled && "pointer-events-none opacity-50",
        className,
      )}
      {...props}
    >
      {on && <span className="absolute left-2 flex h-3.5 w-3.5 items-center justify-center">✓</span>}
      {children}
    </div>
  );
});
export const SelectGroup = simple("div", "", "SelectGroup");
export const SelectLabel = simple("div", "py-1.5 pl-8 pr-2 text-sm font-semibold", "SelectLabel");
export const SelectSeparator = simple("div", "-mx-1 my-1 h-px bg-muted", "SelectSeparator");

// Avatar
export const Avatar = simple("span", "relative flex h-10 w-10 shrink-0 overflow-hidden rounded-full", "Avatar");
export const AvatarImage = ({ className, onError, ...props }) => {
  const [failed, setFailed] = useState(false);
  if (failed) return null;
  return (
    <img
      className={cn("aspect-square h-full w-full", className)}
      onError={(e) => {
        setFailed(true);
        onError?.(e);
      }}
      {...props}
    />
  );
};
export const AvatarFallback = simple(
  "span",
  "flex h-full w-full items-center justify-center rounded-full bg-muted",
  "AvatarFallback",
);

// Table
export const Table = forwardRef(({ className, ...props }, ref) => (
  <div className="relative w-full overflow-auto">
    <table ref={ref} className={cn("w-full caption-bottom text-sm", className)} {...props} />
  </div>
));
export const TableHeader = simple("thead", "[&_tr]:border-b", "TableHeader");
export const TableBody = simple("tbody", "[&_tr:last-child]:border-0", "TableBody");
export const TableFooter = simple("tfoot", "border-t bg-muted/50 font-medium [&>tr]:last:border-b-0", "TableFooter");
export const TableRow = simple("tr", "border-b transition-colors hover:bg-muted/50 data-[state=selected]:bg-muted", "TableRow");
export const TableHead = simple(
  "th",
  "h-12 px-4 text-left align-middle font-medium text-muted-foreground [&:has([role=checkbox])]:pr-0",
  "TableHead",
);
export const TableCell = simple("td", "p-4 align-middle [&:has([role=checkbox])]:pr-0", "TableCell");
export const TableCaption = simple("caption", "mt-4 text-sm text-muted-foreground", "TableCaption");

// Toggle
export const Toggle = forwardRef(
  ({ className, pressed, defaultPressed, onPressedChange, variant = "default", size = "default", ...props }, ref) => {
    const [on, set] = useControllable(pressed, defaultPressed ?? false, onPressedChange);
    return (
      <button
        ref={ref}
        type="button"
        aria-pressed={on}
        data-state={on ? "on" : "off"}
        onClick={() => set(!on)}
        className={cn(
          "inline-flex items-center justify-center gap-2 rounded-md text-sm font-medium transition-colors hover:bg-muted hover:text-muted-foreground disabled:pointer-events-none disabled:opacity-50 cursor-pointer",
          variant === "outline" && "border border-input bg-transparent hover:bg-accent hover:text-accent-foreground",
          { default: "h-10 px-3 min-w-10", sm: "h-9 px-2.5 min-w-9", lg: "h-11 px-5 min-w-11" }[size],
          on && "bg-accent text-accent-foreground",
          className,
        )}
        {...props}
      />
    );
  },
);
